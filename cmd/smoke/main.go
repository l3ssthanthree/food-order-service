package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/avito-intern/kitchen/internal/domain"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	kitchenURL := envOr("KITCHEN_BASE_URL", "http://localhost:8080")
	demoURL := envOr("DEMO_BASE_URL", "http://localhost:8081")
	operatorHeaders := map[string]string{"Authorization": "Bearer " + envOr("DEMO_RESTAURANT_TOKEN", "demo-restaurant-secret")}
	client := &http.Client{Timeout: 5 * time.Second}

	must(waitReady(ctx, client, kitchenURL+"/health/ready"))
	must(waitReady(ctx, client, demoURL+"/health/ready"))

	request := domain.CreateOrderInput{
		RestaurantID:    "00000000-0000-0000-0000-000000000001",
		CustomerName:    "Smoke Test",
		CustomerPhone:   "+79990000000",
		DeliveryAddress: "Москва, ул. Тестовая, 10",
		Items: []domain.CreateOrderItem{
			{MenuItemID: "10000000-0000-0000-0000-000000000001", Quantity: 1},
			{MenuItemID: "10000000-0000-0000-0000-000000000003", Quantity: 2},
		},
	}
	var order domain.Order
	must(doJSON(ctx, client, http.MethodPost, kitchenURL+"/api/v1/orders", map[string]string{
		"X-Avito-User-ID": "smoke-user",
		"Idempotency-Key": fmt.Sprintf("smoke-%d", time.Now().UnixNano()),
	}, request, &order, http.StatusCreated))
	if order.TotalPriceMinor != 109700 || order.Status != domain.OrderPendingRestaurant {
		fail(fmt.Errorf("unexpected created order: total=%d status=%s", order.TotalPriceMinor, order.Status))
	}
	must(waitForRestaurant(ctx, client, demoURL, order.ID, operatorHeaders))

	for _, status := range []domain.OrderStatus{
		domain.OrderAccepted, domain.OrderPreparing, domain.OrderReady, domain.OrderCompleted,
	} {
		var updated domain.Order
		must(doJSON(ctx, client, http.MethodPost, demoURL+"/demo/v1/orders/"+order.ID+"/status", operatorHeaders,
			map[string]domain.OrderStatus{"status": status}, &updated, http.StatusOK))
		if updated.Status != status {
			fail(fmt.Errorf("status update returned %s, want %s", updated.Status, status))
		}
	}

	var final domain.Order
	must(doJSON(ctx, client, http.MethodGet, kitchenURL+"/api/v1/orders/"+order.ID,
		map[string]string{"X-Avito-User-ID": "smoke-user"}, nil, &final, http.StatusOK))
	if final.Status != domain.OrderCompleted {
		fail(fmt.Errorf("final status is %s, want completed", final.Status))
	}
	fmt.Printf("smoke test passed: order %s, total %d %s minor units, status %s\n",
		final.ID, final.TotalPriceMinor, final.Currency, final.Status)
}

func waitReady(ctx context.Context, client *http.Client, endpoint string) error {
	for {
		if err := doJSON(ctx, client, http.MethodGet, endpoint, nil, nil, nil, http.StatusOK); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", endpoint, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForRestaurant(ctx context.Context, client *http.Client, demoURL, orderID string, headers map[string]string) error {
	type orderList struct {
		Orders []domain.Order `json:"orders"`
	}
	for {
		var list orderList
		if err := doJSON(ctx, client, http.MethodGet, demoURL+"/demo/v1/orders", headers, nil, &list, http.StatusOK); err == nil {
			for _, order := range list.Orders {
				if order.ID == orderID {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for restaurant order %s: %w", orderID, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func doJSON(
	ctx context.Context,
	client *http.Client,
	method, endpoint string,
	headers map[string]string,
	requestBody, responseBody any,
	expectedStatus int,
) error {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("%s %s returned %d, want %d: %s",
			method, endpoint, response.StatusCode, expectedStatus, strings.TrimSpace(string(message)))
	}
	if responseBody != nil {
		if err := json.NewDecoder(response.Body).Decode(responseBody); err != nil {
			return fmt.Errorf("decode response from %s: %w", endpoint, err)
		}
	}
	return nil
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "smoke test failed:", err)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return strings.TrimRight(value, "/")
	}
	return fallback
}
