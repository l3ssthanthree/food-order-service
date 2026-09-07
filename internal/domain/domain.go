package domain

import (
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrAlreadyExists       = errors.New("resource already exists")
	ErrForbidden           = errors.New("forbidden")
	ErrInvalidInput        = errors.New("invalid input")
	ErrConflict            = errors.New("conflict")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with another request")
	ErrInsufficientStock   = errors.New("insufficient stock")
	ErrRestaurantClosed    = errors.New("restaurant is closed")
	ErrInvalidTransition   = errors.New("invalid status transition")
)

type OrderStatus string

const (
	OrderPendingRestaurant OrderStatus = "pending_restaurant"
	OrderAccepted          OrderStatus = "accepted"
	OrderRejected          OrderStatus = "rejected"
	OrderPreparing         OrderStatus = "preparing"
	OrderReady             OrderStatus = "ready"
	OrderCompleted         OrderStatus = "completed"
	OrderCancelled         OrderStatus = "cancelled"
)

type Restaurant struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Address     string    `json:"address"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type MenuItem struct {
	ID            string    `json:"id"`
	RestaurantID  string    `json:"restaurant_id"`
	ExternalID    string    `json:"external_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	PriceMinor    int64     `json:"price_minor"`
	Currency      string    `json:"currency"`
	StockQuantity int       `json:"stock_quantity"`
	IsAvailable   bool      `json:"is_available"`
	Version       int64     `json:"version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CreateOrderItem struct {
	MenuItemID string `json:"menu_item_id"`
	Quantity   int    `json:"quantity"`
}

type CreateOrderInput struct {
	RestaurantID    string            `json:"restaurant_id"`
	CustomerName    string            `json:"customer_name"`
	CustomerPhone   string            `json:"customer_phone"`
	DeliveryAddress string            `json:"delivery_address"`
	Items           []CreateOrderItem `json:"items"`
}

type OrderItem struct {
	MenuItemID     string `json:"menu_item_id"`
	ExternalID     string `json:"external_id"`
	Name           string `json:"name"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Quantity       int    `json:"quantity"`
	SubtotalMinor  int64  `json:"subtotal_minor"`
}

type Order struct {
	ID              string      `json:"id"`
	AvitoUserID     string      `json:"avito_user_id"`
	RestaurantID    string      `json:"restaurant_id"`
	Status          OrderStatus `json:"status"`
	CustomerName    string      `json:"customer_name"`
	CustomerPhone   string      `json:"customer_phone"`
	DeliveryAddress string      `json:"delivery_address"`
	TotalPriceMinor int64       `json:"total_price_minor"`
	Currency        string      `json:"currency"`
	Items           []OrderItem `json:"items"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

type UpsertMenuItemInput struct {
	CreateOnly    bool   `json:"-"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	PriceMinor    int64  `json:"price_minor"`
	Currency      string `json:"currency"`
	StockQuantity int    `json:"stock_quantity"`
	IsAvailable   bool   `json:"is_available"`
}

func CanTransition(from, to OrderStatus) bool {
	switch from {
	case OrderPendingRestaurant:
		return to == OrderAccepted || to == OrderRejected || to == OrderCancelled
	case OrderAccepted:
		return to == OrderPreparing
	case OrderPreparing:
		return to == OrderReady
	case OrderReady:
		return to == OrderCompleted
	default:
		return false
	}
}

func IsPartnerStatus(status OrderStatus) bool {
	switch status {
	case OrderAccepted, OrderRejected, OrderPreparing, OrderReady, OrderCompleted:
		return true
	default:
		return false
	}
}
