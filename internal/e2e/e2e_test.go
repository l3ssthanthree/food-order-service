package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/avito-intern/kitchen/internal/demorestaurant"
	"github.com/avito-intern/kitchen/internal/domain"
	"github.com/avito-intern/kitchen/internal/kitchen"
	"github.com/avito-intern/kitchen/internal/outbox"
	"github.com/avito-intern/kitchen/internal/postgres"
	"github.com/avito-intern/kitchen/migrations"
)

// Real PostgreSQL, both HTTP transports and the real outbox worker. Only the
// listener addresses are ephemeral; no persistence or integration mocks.
func TestRestaurantJourney(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for the full HTTP integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := postgres.New(db)
	api := httptest.NewServer(kitchen.NewHandler(kitchen.NewService(store), store, logger))
	defer api.Close()

	token := uuid.NewString()
	digest := sha256.Sum256([]byte(token))
	restaurantID := uuid.NewString()
	demo := demorestaurant.New(demorestaurant.Config{
		KitchenBaseURL: api.URL, PartnerToken: token, CallbackToken: "test-callback",
	}, logger)
	var unavailable atomic.Bool
	unavailable.Store(true)
	demoHandler := demo.Handler()
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/integration/v1/events" && unavailable.Load() {
			http.Error(w, "temporary partner outage", http.StatusServiceUnavailable)
			return
		}
		demoHandler.ServeHTTP(w, r)
	}))
	defer partner.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO restaurants
		(id,name,address,status,callback_url,partner_token_hash)
		VALUES ($1,'E2E cafe','Test address','open',$2,$3)`,
		restaurantID, partner.URL+"/integration/v1/events", hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, query := range []string{
			"DELETE FROM orders WHERE restaurant_id = $1",
			"DELETE FROM menu_items WHERE restaurant_id = $1",
			"DELETE FROM restaurants WHERE id = $1",
		} {
			if _, err := db.Exec(query, restaurantID); err != nil {
				t.Error("cleanup:", err)
			}
		}
	}()
	demo.SyncMenuUntilReady(ctx)
	auth := map[string]string{"Authorization": "Bearer " + token}
	user := map[string]string{"X-Avito-User-ID": "journey-user", "Idempotency-Key": uuid.NewString()}
	call(t, api.URL+"/partner/v1/restaurant/status", "PATCH", nil, map[string]string{"status": "closed"}, 401, nil)
	call(t, partner.URL+"/demo/v1/orders", "GET", nil, nil, 401, nil)
	var menu struct {
		Items []domain.MenuItem `json:"items"`
	}
	call(t, api.URL+"/api/v1/restaurants/"+restaurantID+"/menu", "GET", nil, nil, 200, &menu)
	if len(menu.Items) != 3 {
		t.Fatalf("menu contains %d items, want 3", len(menu.Items))
	}
	var pizza domain.MenuItem
	for _, item := range menu.Items {
		if item.ExternalID == "margherita" {
			pizza = item
		}
	}
	if pizza.ID == "" {
		t.Fatal("missing demo pizza")
	}
	input := domain.CreateOrderInput{
		RestaurantID: restaurantID, CustomerName: "Test customer", CustomerPhone: "+79990000000",
		DeliveryAddress: "Test destination", Items: []domain.CreateOrderItem{{MenuItemID: pizza.ID, Quantity: 2}},
	}
	var created domain.Order
	call(t, api.URL+"/api/v1/orders", "POST", user, input, 201, &created)
	if created.TotalPriceMinor != 119800 {
		t.Fatalf("server total = %d", created.TotalPriceMinor)
	}
	var replay domain.Order
	call(t, api.URL+"/api/v1/orders", "POST", user, input, 200, &replay)
	if replay.ID != created.ID {
		t.Fatal("retry created another order")
	}
	changed := input
	changed.CustomerName = "Different customer"
	call(t, api.URL+"/api/v1/orders", "POST", user, changed, 409, nil)
	call(t, api.URL+"/api/v1/orders/"+created.ID, "GET", map[string]string{"X-Avito-User-ID": "stranger"}, nil, 404, nil)
	// Re-running bootstrap must preserve the reservations.
	demo.SyncMenuUntilReady(ctx)
	var remaining int
	if err := db.QueryRowContext(ctx, "SELECT stock_quantity FROM menu_items WHERE id=$1", pizza.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 18 {
		t.Fatalf("bootstrap overwrote stock: %d", remaining)
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		outbox.NewDispatcher(store, "test-callback", 10*time.Millisecond, logger).Run(workerCtx)
	}()
	defer func() { stopWorker(); <-workerDone }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var attempts int
		if err := db.QueryRowContext(ctx, "SELECT attempts FROM outbox_events WHERE aggregate_id=$1 AND sequence_no=1", created.ID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not try delivery")
		}
		time.Sleep(20 * time.Millisecond)
	}
	unavailable.Store(false)
	awaitOrder(t, partner.URL, auth, created.ID, "pending_restaurant")
	call(t, partner.URL+"/demo/v1/orders/"+created.ID+"/status", "POST", auth, map[string]string{"status": "ready"}, 409, nil)
	for _, status := range []string{"accepted", "preparing", "ready", "completed"} {
		var updated domain.Order
		call(t, partner.URL+"/demo/v1/orders/"+created.ID+"/status", "POST", auth, map[string]string{"status": status}, 200, &updated)
		if string(updated.Status) != status {
			t.Fatalf("wanted %s, got %s", status, updated.Status)
		}
	}
	// A repeated operator click is safe, even though the first update completed.
	call(t, partner.URL+"/demo/v1/orders/"+created.ID+"/status", "POST", auth, map[string]string{"status": "completed"}, 200, nil)
	call(t, api.URL+"/api/v1/orders/"+created.ID+"/cancel", "POST", user, nil, 409, nil)

	// A closed restaurant and a sold-out basket must never create orders.
	call(t, api.URL+"/partner/v1/restaurant/status", "PATCH", auth, map[string]string{"status": "closed"}, 200, nil)
	user["Idempotency-Key"] = uuid.NewString()
	call(t, api.URL+"/api/v1/orders", "POST", user, input, 409, nil)
	call(t, api.URL+"/partner/v1/restaurant/status", "PATCH", auth, map[string]string{"status": "open"}, 200, nil)
	oversized := input
	oversized.Items = []domain.CreateOrderItem{{MenuItemID: pizza.ID, Quantity: 100}}
	call(t, api.URL+"/api/v1/orders", "POST", user, oversized, 409, nil)

	for _, terminal := range []string{"cancelled", "rejected"} {
		user["Idempotency-Key"] = uuid.NewString()
		var order domain.Order
		call(t, api.URL+"/api/v1/orders", "POST", user, input, 201, &order)
		if terminal == "cancelled" {
			call(t, api.URL+"/api/v1/orders/"+order.ID+"/cancel", "POST", user, nil, 200, nil)
			awaitOrder(t, partner.URL, auth, order.ID, terminal)
		} else {
			awaitOrder(t, partner.URL, auth, order.ID, "pending_restaurant")
			call(t, partner.URL+"/demo/v1/orders/"+order.ID+"/status", "POST", auth, map[string]string{"status": terminal}, 200, nil)
		}
		if err := db.QueryRowContext(ctx, "SELECT stock_quantity FROM menu_items WHERE id=$1", pizza.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 18 {
			t.Fatalf("%s did not restore stock: %d", terminal, remaining)
		}
	}
}

func call(t *testing.T, endpoint, method string, headers map[string]string, input any, want int, output any) {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: status %d want %d: %s", method, endpoint, response.StatusCode, want, data)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
}

func awaitOrder(t *testing.T, base string, headers map[string]string, id, status string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		var list struct {
			Orders []domain.Order `json:"orders"`
		}
		call(t, base+"/demo/v1/orders", "GET", headers, nil, 200, &list)
		for _, order := range list.Orders {
			if order.ID == id && string(order.Status) == status {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("restaurant did not receive order %s in status %s", id, status))
}
