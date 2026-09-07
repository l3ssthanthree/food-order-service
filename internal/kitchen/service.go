package kitchen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/avito-intern/kitchen/internal/domain"
)

const (
	maxOrderLines   = 50
	maxItemQuantity = 100
	maxPriceMinor   = 100_000_000
	maxTextLength   = 500
)

type Repository interface {
	ListRestaurants(context.Context) ([]domain.Restaurant, error)
	GetMenu(context.Context, string) ([]domain.MenuItem, error)
	CreateOrder(context.Context, string, string, string, domain.CreateOrderInput) (domain.Order, bool, error)
	GetOrder(context.Context, string) (domain.Order, error)
	CancelOrder(context.Context, string, string, string) (domain.Order, error)
	AuthenticatePartner(context.Context, string) (domain.Restaurant, error)
	UpsertMenuItem(context.Context, string, string, domain.UpsertMenuItemInput) (domain.MenuItem, error)
	SetRestaurantStatus(context.Context, string, string) (domain.Restaurant, error)
	GetPartnerOrder(context.Context, string, string) (domain.Order, error)
	UpdateOrderStatus(context.Context, string, string, string, domain.OrderStatus) (domain.Order, error)
}

type Service struct {
	repo Repository
}

type CreateOrderResult struct {
	Order    domain.Order
	Replayed bool
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) ListRestaurants(ctx context.Context) ([]domain.Restaurant, error) {
	return s.repo.ListRestaurants(ctx)
}

func (s *Service) GetMenu(ctx context.Context, restaurantID string) ([]domain.MenuItem, error) {
	if uuid.Validate(restaurantID) != nil {
		return nil, fmt.Errorf("%w: restaurant_id must be a UUID", domain.ErrInvalidInput)
	}
	return s.repo.GetMenu(ctx, restaurantID)
}

func (s *Service) CreateOrder(
	ctx context.Context,
	userID, idempotencyKey string,
	input domain.CreateOrderInput,
) (CreateOrderResult, error) {
	userID = strings.TrimSpace(userID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if hasNUL(userID, idempotencyKey, input.CustomerName, input.CustomerPhone, input.DeliveryAddress) {
		return CreateOrderResult{}, fmt.Errorf("%w: text cannot contain NUL", domain.ErrInvalidInput)
	}
	if userID == "" || len(userID) > 128 {
		return CreateOrderResult{}, fmt.Errorf("%w: X-Avito-User-ID is required and must be at most 128 bytes", domain.ErrInvalidInput)
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return CreateOrderResult{}, fmt.Errorf("%w: Idempotency-Key is required and must be at most 128 bytes", domain.ErrInvalidInput)
	}
	if uuid.Validate(input.RestaurantID) != nil {
		return CreateOrderResult{}, fmt.Errorf("%w: restaurant_id must be a UUID", domain.ErrInvalidInput)
	}
	input.RestaurantID = uuid.MustParse(input.RestaurantID).String()
	input.CustomerName = strings.TrimSpace(input.CustomerName)
	input.CustomerPhone = strings.TrimSpace(input.CustomerPhone)
	input.DeliveryAddress = strings.TrimSpace(input.DeliveryAddress)
	if input.CustomerName == "" || len(input.CustomerName) > 200 {
		return CreateOrderResult{}, fmt.Errorf("%w: customer_name is required and must be at most 200 bytes", domain.ErrInvalidInput)
	}
	if input.CustomerPhone == "" || len(input.CustomerPhone) > 32 {
		return CreateOrderResult{}, fmt.Errorf("%w: customer_phone is required and must be at most 32 bytes", domain.ErrInvalidInput)
	}
	if input.DeliveryAddress == "" || len(input.DeliveryAddress) > maxTextLength {
		return CreateOrderResult{}, fmt.Errorf("%w: delivery_address is required and must be at most 500 bytes", domain.ErrInvalidInput)
	}
	items, err := normalizeItems(input.Items)
	if err != nil {
		return CreateOrderResult{}, err
	}
	input.Items = items
	payload, err := json.Marshal(input)
	if err != nil {
		return CreateOrderResult{}, fmt.Errorf("marshal request for idempotency: %w", err)
	}
	digest := sha256.Sum256(payload)
	order, replayed, err := s.repo.CreateOrder(ctx, userID, idempotencyKey, hex.EncodeToString(digest[:]), input)
	return CreateOrderResult{Order: order, Replayed: replayed}, err
}

func (s *Service) GetOrder(ctx context.Context, userID, orderID string) (domain.Order, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return domain.Order{}, fmt.Errorf("%w: X-Avito-User-ID is required", domain.ErrInvalidInput)
	}
	if uuid.Validate(orderID) != nil {
		return domain.Order{}, fmt.Errorf("%w: order_id must be a UUID", domain.ErrInvalidInput)
	}
	order, err := s.repo.GetOrder(ctx, orderID)
	if err != nil {
		return domain.Order{}, err
	}
	if order.AvitoUserID != userID {
		return domain.Order{}, domain.ErrNotFound
	}
	return order, nil
}

func (s *Service) CancelOrder(ctx context.Context, userID, orderID string) (domain.Order, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return domain.Order{}, fmt.Errorf("%w: X-Avito-User-ID is required", domain.ErrInvalidInput)
	}
	if uuid.Validate(orderID) != nil {
		return domain.Order{}, fmt.Errorf("%w: order_id must be a UUID", domain.ErrInvalidInput)
	}
	return s.repo.CancelOrder(ctx, userID, orderID, uuid.NewString())
}

func (s *Service) AuthenticatePartner(ctx context.Context, token string) (domain.Restaurant, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return domain.Restaurant{}, domain.ErrForbidden
	}
	digest := sha256.Sum256([]byte(token))
	return s.repo.AuthenticatePartner(ctx, hex.EncodeToString(digest[:]))
}

func (s *Service) UpsertMenuItem(
	ctx context.Context,
	restaurantID, externalID string,
	input domain.UpsertMenuItemInput,
) (domain.MenuItem, error) {
	externalID = strings.TrimSpace(externalID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	if hasNUL(externalID, input.Name, input.Description) {
		return domain.MenuItem{}, fmt.Errorf("%w: text cannot contain NUL", domain.ErrInvalidInput)
	}
	if externalID == "" || len(externalID) > 128 {
		return domain.MenuItem{}, fmt.Errorf("%w: external_id is required and must be at most 128 bytes", domain.ErrInvalidInput)
	}
	if input.Name == "" || len(input.Name) > 200 {
		return domain.MenuItem{}, fmt.Errorf("%w: name is required and must be at most 200 bytes", domain.ErrInvalidInput)
	}
	if len(input.Description) > 1000 {
		return domain.MenuItem{}, fmt.Errorf("%w: description must be at most 1000 bytes", domain.ErrInvalidInput)
	}
	if input.PriceMinor <= 0 || input.PriceMinor > maxPriceMinor {
		return domain.MenuItem{}, fmt.Errorf("%w: price_minor must be between 1 and %d", domain.ErrInvalidInput, maxPriceMinor)
	}
	if input.Currency != "RUB" {
		return domain.MenuItem{}, fmt.Errorf("%w: MVP supports RUB only", domain.ErrInvalidInput)
	}
	if input.StockQuantity < 0 || input.StockQuantity > 1_000_000 {
		return domain.MenuItem{}, fmt.Errorf("%w: stock_quantity must be between 0 and 1000000", domain.ErrInvalidInput)
	}
	return s.repo.UpsertMenuItem(ctx, restaurantID, externalID, input)
}

func (s *Service) SetRestaurantStatus(ctx context.Context, restaurantID, status string) (domain.Restaurant, error) {
	if status != "open" && status != "closed" {
		return domain.Restaurant{}, fmt.Errorf("%w: status must be open or closed", domain.ErrInvalidInput)
	}
	return s.repo.SetRestaurantStatus(ctx, restaurantID, status)
}

func (s *Service) GetPartnerOrder(ctx context.Context, restaurantID, orderID string) (domain.Order, error) {
	if uuid.Validate(orderID) != nil {
		return domain.Order{}, fmt.Errorf("%w: order_id must be a UUID", domain.ErrInvalidInput)
	}
	return s.repo.GetPartnerOrder(ctx, restaurantID, orderID)
}

func (s *Service) UpdateOrderStatus(
	ctx context.Context,
	restaurantID, orderID, eventID string,
	status domain.OrderStatus,
) (domain.Order, error) {
	if uuid.Validate(orderID) != nil {
		return domain.Order{}, fmt.Errorf("%w: order_id must be a UUID", domain.ErrInvalidInput)
	}
	if uuid.Validate(eventID) != nil {
		return domain.Order{}, fmt.Errorf("%w: event_id must be a UUID", domain.ErrInvalidInput)
	}
	orderID = uuid.MustParse(orderID).String()
	eventID = uuid.MustParse(eventID).String()
	if !domain.IsPartnerStatus(status) {
		return domain.Order{}, fmt.Errorf("%w: unsupported partner status", domain.ErrInvalidInput)
	}
	return s.repo.UpdateOrderStatus(ctx, restaurantID, orderID, eventID, status)
}

func normalizeItems(items []domain.CreateOrderItem) ([]domain.CreateOrderItem, error) {
	if len(items) == 0 || len(items) > maxOrderLines {
		return nil, fmt.Errorf("%w: items must contain between 1 and %d lines", domain.ErrInvalidInput, maxOrderLines)
	}
	quantities := make(map[string]int, len(items))
	for _, item := range items {
		if uuid.Validate(item.MenuItemID) != nil {
			return nil, fmt.Errorf("%w: every menu_item_id must be a UUID", domain.ErrInvalidInput)
		}
		item.MenuItemID = uuid.MustParse(item.MenuItemID).String()
		if item.Quantity <= 0 || item.Quantity > maxItemQuantity {
			return nil, fmt.Errorf("%w: quantity must be between 1 and %d", domain.ErrInvalidInput, maxItemQuantity)
		}
		quantities[item.MenuItemID] += item.Quantity
		if quantities[item.MenuItemID] > maxItemQuantity {
			return nil, fmt.Errorf("%w: total quantity per item must not exceed %d", domain.ErrInvalidInput, maxItemQuantity)
		}
	}
	normalized := make([]domain.CreateOrderItem, 0, len(quantities))
	for itemID, quantity := range quantities {
		normalized = append(normalized, domain.CreateOrderItem{MenuItemID: itemID, Quantity: quantity})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].MenuItemID < normalized[j].MenuItemID })
	return normalized, nil
}

func hasNUL(values ...string) bool {
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return true
		}
	}
	return false
}
