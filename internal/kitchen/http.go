package kitchen

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/avito-intern/kitchen/internal/domain"
	"github.com/avito-intern/kitchen/internal/httpx"
)

type Pinger interface {
	Ping(context.Context) error
}

type Handler struct {
	service *Service
	pinger  Pinger
	logger  *slog.Logger
}

func NewHandler(service *Service, pinger Pinger, logger *slog.Logger) http.Handler {
	h := &Handler{service: service, pinger: pinger, logger: logger}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(15 * time.Second))
	router.Use(h.accessLog)
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	})
	router.MethodNotAllowed(httpx.MethodNotAllowed)

	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/health/ready", h.ready)

	router.Route("/api/v1", func(r chi.Router) {
		r.Get("/restaurants", h.listRestaurants)
		r.Get("/restaurants/{restaurantID}/menu", h.getMenu)
		r.Post("/orders", h.createOrder)
		r.Get("/orders/{orderID}", h.getOrder)
		r.Post("/orders/{orderID}/cancel", h.cancelOrder)
	})
	router.Route("/partner/v1", func(r chi.Router) {
		r.Put("/menu-items/{externalID}", h.upsertMenuItem)
		r.Patch("/restaurant/status", h.setRestaurantStatus)
		r.Get("/orders/{orderID}", h.getPartnerOrder)
		r.Post("/orders/{orderID}/status", h.updateOrderStatus)
	})
	return router
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.pinger.Ping(ctx); err != nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "not_ready", "database is unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) listRestaurants(w http.ResponseWriter, r *http.Request) {
	restaurants, err := h.service.ListRestaurants(r.Context())
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"restaurants": restaurants})
}

func (h *Handler) getMenu(w http.ResponseWriter, r *http.Request) {
	items, err := h.service.GetMenu(r.Context(), chi.URLParam(r, "restaurantID"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var input domain.CreateOrderInput
	if err := httpx.DecodeJSON(w, r, &input); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := h.service.CreateOrder(
		r.Context(), r.Header.Get("X-Avito-User-ID"), r.Header.Get("Idempotency-Key"), input,
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+result.Order.ID)
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	httpx.WriteJSON(w, status, result.Order)
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	order, err := h.service.GetOrder(
		r.Context(), r.Header.Get("X-Avito-User-ID"), chi.URLParam(r, "orderID"),
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	order, err := h.service.CancelOrder(
		r.Context(), r.Header.Get("X-Avito-User-ID"), chi.URLParam(r, "orderID"),
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) upsertMenuItem(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := h.requirePartner(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		PriceMinor    int64  `json:"price_minor"`
		Currency      string `json:"currency"`
		StockQuantity *int   `json:"stock_quantity"`
		IsAvailable   *bool  `json:"is_available"`
	}
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.StockQuantity == nil || body.IsAvailable == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_input", "stock_quantity and is_available are required")
		return
	}
	condition := r.Header.Get("If-None-Match")
	if condition != "" && condition != "*" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_input", "If-None-Match supports only *")
		return
	}
	input := domain.UpsertMenuItemInput{
		Name: body.Name, Description: body.Description, PriceMinor: body.PriceMinor,
		Currency: body.Currency, StockQuantity: *body.StockQuantity, IsAvailable: *body.IsAvailable,
		CreateOnly: condition == "*",
	}
	item, err := h.service.UpsertMenuItem(
		r.Context(), restaurant.ID, chi.URLParam(r, "externalID"), input,
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, item)
}

func (h *Handler) setRestaurantStatus(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := h.requirePartner(w, r)
	if !ok {
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if err := httpx.DecodeJSON(w, r, &input); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	updated, err := h.service.SetRestaurantStatus(r.Context(), restaurant.ID, input.Status)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, updated)
}

func (h *Handler) getPartnerOrder(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := h.requirePartner(w, r)
	if !ok {
		return
	}
	order, err := h.service.GetPartnerOrder(r.Context(), restaurant.ID, chi.URLParam(r, "orderID"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) updateOrderStatus(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := h.requirePartner(w, r)
	if !ok {
		return
	}
	var input struct {
		EventID string             `json:"event_id"`
		Status  domain.OrderStatus `json:"status"`
	}
	if err := httpx.DecodeJSON(w, r, &input); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	order, err := h.service.UpdateOrderStatus(
		r.Context(), restaurant.ID, chi.URLParam(r, "orderID"), input.EventID, input.Status,
	)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, order)
}

func (h *Handler) requirePartner(w http.ResponseWriter, r *http.Request) (domain.Restaurant, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid partner token is required")
		return domain.Restaurant{}, false
	}
	restaurant, err := h.service.AuthenticatePartner(r.Context(), strings.TrimPrefix(header, prefix))
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid partner token is required")
			return domain.Restaurant{}, false
		}
		h.writeServiceError(w, r, err)
		return domain.Restaurant{}, false
	}
	return restaurant, true
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrAlreadyExists):
		httpx.WriteError(w, http.StatusPreconditionFailed, "already_exists", "menu item already exists")
	case errors.Is(err, domain.ErrInvalidInput):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "operation is forbidden")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		httpx.WriteError(w, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, domain.ErrInsufficientStock):
		httpx.WriteError(w, http.StatusConflict, "insufficient_stock", err.Error())
	case errors.Is(err, domain.ErrRestaurantClosed):
		httpx.WriteError(w, http.StatusConflict, "restaurant_closed", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		httpx.WriteError(w, http.StatusConflict, "invalid_status_transition", err.Error())
	case errors.Is(err, domain.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "conflict", err.Error())
	default:
		h.logger.Error("request failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func (h *Handler) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		h.logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", middleware.GetReqID(r.Context()),
			"duration", time.Since(started),
		)
	})
}
