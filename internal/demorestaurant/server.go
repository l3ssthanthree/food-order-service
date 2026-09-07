package demorestaurant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/avito-intern/kitchen/internal/domain"
	"github.com/avito-intern/kitchen/internal/httpx"
)

type Config struct {
	KitchenBaseURL string
	PartnerToken   string
	CallbackToken  string
}

type Server struct {
	config   Config
	client   *http.Client
	logger   *slog.Logger
	ready    atomic.Bool
	mu       sync.RWMutex
	orders   map[string]domain.Order
	received map[string]struct{}
}

type integrationEvent struct {
	EventID    string       `json:"event_id"`
	EventType  string       `json:"event_type"`
	OccurredAt time.Time    `json:"occurred_at"`
	Order      domain.Order `json:"order"`
}

func New(config Config, logger *slog.Logger) *Server {
	return &Server{
		config:   config,
		client:   &http.Client{Timeout: 5 * time.Second},
		logger:   logger,
		orders:   make(map[string]domain.Order),
		received: make(map[string]struct{}),
	}
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(10 * time.Second))
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	})
	router.MethodNotAllowed(httpx.MethodNotAllowed)
	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			httpx.WriteError(w, http.StatusServiceUnavailable, "not_ready", "menu has not been synchronized")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Post("/integration/v1/events", s.receiveEvent)
	router.Route("/demo/v1", func(r chi.Router) {
		r.Use(s.requireOperator)
		r.Get("/orders", s.listOrders)
		r.Post("/orders/{orderID}/status", s.changeOrderStatus)
	})
	return router
}

func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !constantTimeTokenEqual(bearerToken(r.Header.Get("Authorization")), s.config.PartnerToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid partner token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) SyncMenuUntilReady(ctx context.Context) {
	items := []struct {
		externalID string
		input      domain.UpsertMenuItemInput
	}{
		{"margherita", domain.UpsertMenuItemInput{Name: "Пицца Маргарита", Description: "Томаты, моцарелла, базилик", PriceMinor: 59900, Currency: "RUB", StockQuantity: 20, IsAvailable: true}},
		{"pepperoni", domain.UpsertMenuItemInput{Name: "Пицца Пепперони", Description: "Пепперони, моцарелла, томатный соус", PriceMinor: 74900, Currency: "RUB", StockQuantity: 15, IsAvailable: true}},
		{"lemonade", domain.UpsertMenuItemInput{Name: "Домашний лимонад", Description: "Лимон, мята, содовая", PriceMinor: 24900, Currency: "RUB", StockQuantity: 30, IsAvailable: true}},
	}
	for {
		allSynced := true
		for _, item := range items {
			if err := s.syncMenuItem(ctx, item.externalID, item.input); err != nil {
				allSynced = false
				s.logger.Warn("menu synchronization failed", "external_id", item.externalID, "error", err)
				break
			}
		}
		if allSynced {
			s.ready.Store(true)
			s.logger.Info("demo restaurant menu synchronized")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Server) receiveEvent(w http.ResponseWriter, r *http.Request) {
	if !constantTimeTokenEqual(bearerToken(r.Header.Get("Authorization")), s.config.CallbackToken) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid callback token is required")
		return
	}
	var event integrationEvent
	if err := httpx.DecodeJSON(w, r, &event); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if uuid.Validate(event.EventID) != nil || uuid.Validate(event.Order.ID) != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_event", "event_id and order.id must be UUIDs")
		return
	}
	if event.EventType != "order.created" && event.EventType != "order.cancelled" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_event", "unsupported event_type")
		return
	}
	if event.Order.UpdatedAt.IsZero() || event.OccurredAt.IsZero() ||
		(event.EventType == "order.created" && event.Order.Status != domain.OrderPendingRestaurant) ||
		(event.EventType == "order.cancelled" && event.Order.Status != domain.OrderCancelled) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_event", "event timestamp and matching order status are required")
		return
	}
	s.mu.Lock()
	if _, duplicate := s.received[event.EventID]; duplicate {
		s.mu.Unlock()
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "already_processed"})
		return
	}
	// A delayed creation event must not overwrite a newer status returned by Kitchen.
	s.mergeOrderLocked(event.Order)
	s.received[event.EventID] = struct{}{}
	s.mu.Unlock()
	s.logger.Info("integration event received", "event_id", event.EventID, "event_type", event.EventType, "order_id", event.Order.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) listOrders(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	orders := make([]domain.Order, 0, len(s.orders))
	for _, order := range s.orders {
		orders = append(orders, order)
	}
	s.mu.RUnlock()
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.After(orders[j].CreatedAt) })
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

func (s *Server) changeOrderStatus(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "orderID")
	if uuid.Validate(orderID) != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_input", "order_id must be a UUID")
		return
	}
	orderID = uuid.MustParse(orderID).String()
	var input struct {
		Status domain.OrderStatus `json:"status"`
	}
	if err := httpx.DecodeJSON(w, r, &input); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !domain.IsPartnerStatus(input.Status) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_input", "unsupported status")
		return
	}
	// Kitchen is authoritative. This also recovers a known order after a demo restart
	// or when a previous status update committed but its HTTP response was lost.
	current, err := s.getOrder(r.Context(), orderID)
	if err != nil {
		s.writeKitchenError(w, err)
		return
	}
	s.mu.Lock()
	s.mergeOrderLocked(current)
	s.mu.Unlock()
	if current.Status == input.Status {
		httpx.WriteJSON(w, http.StatusOK, current)
		return
	}
	if !domain.CanTransition(current.Status, input.Status) {
		httpx.WriteError(w, http.StatusConflict, "invalid_status_transition", "invalid status transition")
		return
	}
	updated, err := s.sendStatus(r.Context(), orderID, input.Status)
	if err != nil {
		if reconciled, reconcileErr := s.getOrder(r.Context(), orderID); reconcileErr == nil {
			s.mu.Lock()
			s.mergeOrderLocked(reconciled)
			s.mu.Unlock()
			if reconciled.Status == input.Status {
				httpx.WriteJSON(w, http.StatusOK, reconciled)
				return
			}
		}
		s.logger.Error("send order status", "order_id", orderID, "error", err)
		s.writeKitchenError(w, err)
		return
	}
	s.mu.Lock()
	s.mergeOrderLocked(updated)
	s.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, updated)
}

func (s *Server) syncMenuItem(ctx context.Context, externalID string, input domain.UpsertMenuItemInput) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(s.config.KitchenBaseURL, "/") + "/partner/v1/menu-items/" + url.PathEscape(externalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.config.PartnerToken)
	// Seeding is create-only: a restart must not replenish stock or overwrite edits.
	req.Header.Set("If-None-Match", "*")
	response, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusPreconditionFailed {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("Kitchen returned %d: %s", response.StatusCode, bytes.TrimSpace(message))
	}
	return nil
}

func (s *Server) sendStatus(ctx context.Context, orderID string, status domain.OrderStatus) (domain.Order, error) {
	body, err := json.Marshal(struct {
		EventID string             `json:"event_id"`
		Status  domain.OrderStatus `json:"status"`
	}{EventID: uuid.NewString(), Status: status})
	if err != nil {
		return domain.Order{}, err
	}
	// Marshal once so retries use exactly the same event_id and payload.
	for attempt := 0; attempt < 3; attempt++ {
		order, sendErr := s.orderRequest(ctx, http.MethodPost, orderID, "/status", body)
		if sendErr == nil {
			return order, nil
		}
		err = sendErr
		var upstream *kitchenError
		if errors.As(err, &upstream) && upstream.status < 500 && upstream.status != http.StatusTooManyRequests {
			break
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return domain.Order{}, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
	}
	return domain.Order{}, err
}

func (s *Server) getOrder(ctx context.Context, orderID string) (domain.Order, error) {
	return s.orderRequest(ctx, http.MethodGet, orderID, "", nil)
}

type kitchenError struct {
	status int
}

func (err *kitchenError) Error() string {
	return fmt.Sprintf("Kitchen returned %d", err.status)
}

func (s *Server) orderRequest(ctx context.Context, method, orderID, suffix string, body []byte) (domain.Order, error) {
	endpoint := strings.TrimRight(s.config.KitchenBaseURL, "/") + "/partner/v1/orders/" + orderID + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.Order{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.config.PartnerToken)
	response, err := s.client.Do(req)
	if err != nil {
		return domain.Order{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return domain.Order{}, &kitchenError{status: response.StatusCode}
	}
	var order domain.Order
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&order); err != nil {
		return domain.Order{}, fmt.Errorf("decode Kitchen response: %w", err)
	}
	if order.ID != orderID || order.UpdatedAt.IsZero() {
		return domain.Order{}, fmt.Errorf("invalid Kitchen order response")
	}
	return order, nil
}

func (s *Server) writeKitchenError(w http.ResponseWriter, err error) {
	var upstream *kitchenError
	if errors.As(err, &upstream) {
		switch upstream.status {
		case http.StatusNotFound:
			httpx.WriteError(w, http.StatusNotFound, "not_found", "order not found")
			return
		case http.StatusConflict:
			httpx.WriteError(w, http.StatusConflict, "invalid_status_transition", "invalid status transition")
			return
		}
	}
	httpx.WriteError(w, http.StatusBadGateway, "kitchen_unavailable", "Kitchen API is unavailable; retry the same desired status")
}

// Caller holds s.mu. Equal timestamps keep the existing snapshot.
func (s *Server) mergeOrderLocked(order domain.Order) {
	current, exists := s.orders[order.ID]
	if !exists || order.UpdatedAt.After(current.UpdatedAt) {
		s.orders[order.ID] = order
	}
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimPrefix(header, prefix)
}

func constantTimeTokenEqual(got, expected string) bool {
	gotHash := sha256.Sum256([]byte(got))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1 && got != ""
}
