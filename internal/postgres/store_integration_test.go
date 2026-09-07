package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/avito-intern/kitchen/internal/domain"
	"github.com/avito-intern/kitchen/internal/kitchen"
	"github.com/avito-intern/kitchen/internal/postgres"
	"github.com/avito-intern/kitchen/migrations"
)

const demoRestaurantID = "00000000-0000-0000-0000-000000000001"

func TestOrderPersistenceInvariants(t *testing.T) {
	db, ctx := integrationDatabase(t, true)
	repo := postgres.New(db)
	service := kitchen.NewService(repo)

	externalID := "integration-" + uuid.NewString()
	menuItem, err := repo.UpsertMenuItem(ctx, demoRestaurantID, externalID, domain.UpsertMenuItemInput{
		Name: "Integration item", PriceMinor: 12345, Currency: "RUB",
		StockQuantity: 5, IsAvailable: true,
	})
	if err != nil {
		t.Fatalf("create integration menu item: %v", err)
	}
	defer func() {
		if _, cleanupErr := db.ExecContext(context.Background(),
			"DELETE FROM menu_items WHERE id = $1", menuItem.ID,
		); cleanupErr != nil {
			t.Logf("cleanup menu item: %v", cleanupErr)
		}
	}()

	userID := "integration-user-" + uuid.NewString()
	input := orderInput(menuItem.ID, 2)
	created, err := service.CreateOrder(ctx, userID, "same-key", input)
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	defer deleteOrder(t, db, created.Order.ID)
	if created.Replayed {
		t.Fatal("first create was marked as replay")
	}
	if created.Order.TotalPriceMinor != 24690 {
		t.Fatalf("total = %d, want 24690", created.Order.TotalPriceMinor)
	}
	assertStock(t, ctx, db, menuItem.ID, 3)

	replayed, err := service.CreateOrder(ctx, userID, "same-key", input)
	if err != nil {
		t.Fatalf("replay order: %v", err)
	}
	if !replayed.Replayed || replayed.Order.ID != created.Order.ID {
		t.Fatalf("replay = %+v, want original order %s", replayed, created.Order.ID)
	}
	assertStock(t, ctx, db, menuItem.ID, 3)

	_, err = service.CreateOrder(ctx, userID, "same-key", orderInput(menuItem.ID, 1))
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("changed replay error = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := service.CancelOrder(ctx, userID, created.Order.ID); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	assertStock(t, ctx, db, menuItem.ID, 5)

	if _, err := repo.UpsertMenuItem(ctx, demoRestaurantID, externalID, domain.UpsertMenuItemInput{
		Name: "Integration item", PriceMinor: 12345, Currency: "RUB",
		StockQuantity: 1, IsAvailable: true,
	}); err != nil {
		t.Fatalf("reset stock for concurrency check: %v", err)
	}

	type result struct {
		orderID string
		err     error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			start.Wait()
			created, createErr := service.CreateOrder(
				ctx,
				fmt.Sprintf("parallel-user-%d-%s", index, uuid.NewString()),
				"last-item",
				orderInput(menuItem.ID, 1),
			)
			results <- result{orderID: created.Order.ID, err: createErr}
		}()
	}
	start.Done()

	var successes, insufficient int
	for count := 0; count < 2; count++ {
		result := <-results
		switch {
		case result.err == nil:
			successes++
			defer deleteOrder(t, db, result.orderID)
		case errors.Is(result.err, domain.ErrInsufficientStock):
			insufficient++
		default:
			t.Fatalf("parallel create returned unexpected error: %v", result.err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("parallel results: successes=%d insufficient=%d, want 1 and 1", successes, insufficient)
	}
	assertStock(t, ctx, db, menuItem.ID, 0)
}

func TestConcurrentIdempotencyAndPartnerEventOwnership(t *testing.T) {
	db, ctx := integrationDatabase(t, true)
	repo := postgres.New(db)
	service := kitchen.NewService(repo)
	item := createIntegrationItem(t, ctx, repo, "parallel-replay", 5)
	userID := uuid.NewString()
	type result struct {
		created kitchen.CreateOrderResult
		err     error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			created, err := service.CreateOrder(ctx, userID, "same-key", orderInput(item.ID, 2))
			results <- result{created: created, err: err}
		}()
	}
	close(start)
	var orderID string
	var fresh int
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent create: %v", result.err)
		}
		if orderID == "" {
			orderID = result.created.Order.ID
		}
		if result.created.Order.ID != orderID {
			t.Fatalf("duplicate order: %s, want %s", result.created.Order.ID, orderID)
		}
		if !result.created.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh orders = %d, want 1", fresh)
	}
	assertStock(t, ctx, db, item.ID, 3)
	assertCount(t, ctx, db, "SELECT count(*) FROM outbox_events", 1)

	eventID := uuid.NewString()
	statusErrors := make(chan error, 8)
	for i := 0; i < cap(statusErrors); i++ {
		go func() {
			_, err := service.UpdateOrderStatus(ctx, demoRestaurantID, orderID, eventID, domain.OrderRejected)
			statusErrors <- err
		}()
	}
	for i := 0; i < cap(statusErrors); i++ {
		if err := <-statusErrors; err != nil {
			t.Fatalf("concurrent partner event: %v", err)
		}
	}
	assertStock(t, ctx, db, item.ID, 5)
	assertCount(t, ctx, db, "SELECT count(*) FROM order_status_events WHERE actor = 'restaurant'", 1)

	if _, err := service.UpdateOrderStatus(ctx, uuid.NewString(), orderID, eventID, domain.OrderRejected); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign restaurant event replay = %v, want ErrNotFound", err)
	}
	if _, err := service.UpdateOrderStatus(ctx, demoRestaurantID, orderID, eventID, domain.OrderAccepted); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("changed status event replay = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := service.CancelOrder(ctx, userID, orderID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("cancel rejected order = %v, want ErrInvalidTransition", err)
	}
	assertStock(t, ctx, db, item.ID, 5)
}

func TestOrderRollbackSnapshotsAndCreateOnlyMenu(t *testing.T) {
	db, ctx := integrationDatabase(t, true)
	repo := postgres.New(db)
	service := kitchen.NewService(repo)
	first := createIntegrationItem(t, ctx, repo, "snapshot-a", 3)
	second := createIntegrationItem(t, ctx, repo, "snapshot-b", 0)
	input := orderInput(first.ID, 1)
	input.Items = append(input.Items, domain.CreateOrderItem{MenuItemID: second.ID, Quantity: 1})
	userID := uuid.NewString()
	if _, err := service.CreateOrder(ctx, userID, "rollback", input); !errors.Is(err, domain.ErrInsufficientStock) {
		t.Fatalf("unavailable order = %v, want ErrInsufficientStock", err)
	}
	assertStock(t, ctx, db, first.ID, 3)
	assertCount(t, ctx, db, "SELECT count(*) FROM orders", 0)
	assertCount(t, ctx, db, "SELECT count(*) FROM outbox_events", 0)

	created, err := service.CreateOrder(ctx, userID, "rollback", orderInput(first.ID, 1))
	if err != nil {
		t.Fatalf("reuse key after rolled back request: %v", err)
	}
	assertStock(t, ctx, db, first.ID, 2)

	menuInput := domain.UpsertMenuItemInput{
		Name: "Changed name", PriceMinor: 99999, Currency: "RUB",
		StockQuantity: 100, IsAvailable: true, CreateOnly: true,
	}
	if _, err := repo.UpsertMenuItem(ctx, demoRestaurantID, first.ExternalID, menuInput); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create-only existing menu item = %v, want ErrAlreadyExists", err)
	}
	assertStock(t, ctx, db, first.ID, 2)
	if _, err := repo.UpsertMenuItem(ctx, demoRestaurantID, "new-create-only", menuInput); err != nil {
		t.Fatalf("create-only new menu item: %v", err)
	}
	menuInput.CreateOnly = false
	if _, err := repo.UpsertMenuItem(ctx, demoRestaurantID, first.ExternalID, menuInput); err != nil {
		t.Fatalf("update menu item: %v", err)
	}
	saved, err := service.GetOrder(ctx, userID, created.Order.ID)
	if err != nil {
		t.Fatalf("get saved snapshot: %v", err)
	}
	if len(saved.Items) != 1 || saved.Items[0].Name != first.Name || saved.Items[0].UnitPriceMinor != first.PriceMinor {
		t.Fatalf("menu update changed order snapshot: %+v", saved.Items)
	}
	if _, err := service.GetOrder(ctx, "other-user", saved.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign user read = %v, want ErrNotFound", err)
	}
	if _, err := service.CancelOrder(ctx, "other-user", saved.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign user cancellation = %v, want ErrNotFound", err)
	}
	cancelled, err := service.CancelOrder(ctx, userID, saved.ID)
	if err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	persisted, err := service.GetOrder(ctx, userID, saved.ID)
	if err != nil {
		t.Fatalf("read cancelled order: %v", err)
	}
	if !cancelled.UpdatedAt.Equal(persisted.UpdatedAt) {
		t.Fatalf("returned cancellation timestamp differs from stored timestamp: %s vs %s", cancelled.UpdatedAt, persisted.UpdatedAt)
	}
}

func TestOutboxOrderingLeaseFencingAndErrorEncoding(t *testing.T) {
	db, ctx := integrationDatabase(t, true)
	repo := postgres.New(db)
	service := kitchen.NewService(repo)
	item := createIntegrationItem(t, ctx, repo, "outbox", 2)
	created, err := service.CreateOrder(ctx, "outbox-user", "create", orderInput(item.ID, 1))
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := service.CancelOrder(ctx, "outbox-user", created.Order.ID); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	first, found, err := repo.ClaimOutbox(ctx)
	if err != nil || !found || first.Attempts != 1 {
		t.Fatalf("first claim = %+v, found=%v, err=%v", first, found, err)
	}
	if _, found, err := repo.ClaimOutbox(ctx); err != nil || found {
		t.Fatalf("later event passed its predecessor: found=%v err=%v", found, err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE outbox_events SET locked_at = now() - interval '2 minutes' WHERE id = $1", first.ID); err != nil {
		t.Fatalf("expire event lease: %v", err)
	}
	reclaimed, found, err := repo.ClaimOutbox(ctx)
	if err != nil || !found || reclaimed.ID != first.ID || reclaimed.Attempts != 2 {
		t.Fatalf("reclaimed event = %+v, found=%v, err=%v", reclaimed, found, err)
	}
	if err := repo.MarkOutboxFailed(ctx, first.ID, first.Attempts, "stale worker", time.Now()); err != nil {
		t.Fatalf("stale failure: %v", err)
	}
	if err := repo.MarkOutboxDelivered(ctx, first.ID, first.Attempts); err != nil {
		t.Fatalf("stale success: %v", err)
	}
	assertCount(t, ctx, db, "SELECT count(*) FROM outbox_events WHERE status = 'processing' AND attempts = 2", 1)
	if err := repo.MarkOutboxDelivered(ctx, reclaimed.ID, reclaimed.Attempts); err != nil {
		t.Fatalf("current success: %v", err)
	}
	second, found, err := repo.ClaimOutbox(ctx)
	if err != nil || !found || second.ID == first.ID {
		t.Fatalf("successor claim = %+v, found=%v, err=%v", second, found, err)
	}
	message := "bad\x00\xff" + strings.Repeat("я", 1100)
	if err := repo.MarkOutboxFailed(ctx, second.ID, second.Attempts, message, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("persist arbitrary callback error: %v", err)
	}
	var diagnostic string
	if err := db.QueryRowContext(ctx, "SELECT last_error FROM outbox_events WHERE id = $1", second.ID).Scan(&diagnostic); err != nil {
		t.Fatalf("read callback diagnostic: %v", err)
	}
	if len([]rune(diagnostic)) != 1000 || !strings.HasPrefix(diagnostic, "bad??") {
		t.Fatalf("unexpected diagnostic prefix/length: %q / %d", diagnostic[:10], len([]rune(diagnostic)))
	}
	// A late failure must not resurrect an already delivered predecessor.
	if err := repo.MarkOutboxFailed(ctx, reclaimed.ID, reclaimed.Attempts, "too late", time.Now()); err != nil {
		t.Fatalf("failure after success: %v", err)
	}
	assertCount(t, ctx, db, "SELECT count(*) FROM outbox_events WHERE status = 'delivered'", 1)
}

func TestConcurrentMigrationsAreAtomicAndRepeatable(t *testing.T) {
	db, ctx := integrationDatabase(t, false)
	results := make(chan error, 6)
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			results <- migrations.Up(ctx, db)
		}()
	}
	close(start)
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	assertCount(t, ctx, db, "SELECT count(*) FROM schema_migrations", 1)
	assertCount(t, ctx, db, "SELECT count(*) FROM restaurants", 1)
	assertCount(t, ctx, db, "SELECT count(*) FROM menu_items", 3)
}

// Each test owns a fresh schema. TEST_DATABASE_URL may therefore point to a
// database used by other local tests without modifying their tables or data.
func integrationDatabase(t *testing.T, migrate bool) (*sql.DB, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	admin := stdlib.OpenDB(*config)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	schema := "integration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated integration schema: %v", err)
	}
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		_ = db.Close()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelCleanup()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("clean isolated integration schema: %v", err)
		}
		_ = admin.Close()
	})
	if migrate {
		if err := migrations.Up(ctx, db); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	}
	return db, ctx
}

func createIntegrationItem(t *testing.T, ctx context.Context, repo *postgres.Store, externalID string, stock int) domain.MenuItem {
	t.Helper()
	item, err := repo.UpsertMenuItem(ctx, demoRestaurantID, externalID, domain.UpsertMenuItemInput{
		Name: "Integration item", PriceMinor: 12345, Currency: "RUB", StockQuantity: stock, IsAvailable: true,
	})
	if err != nil {
		t.Fatalf("create integration menu item: %v", err)
	}
	return item
}

func assertCount(t *testing.T, ctx context.Context, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, query).Scan(&got); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if got != want {
		t.Fatalf("row count = %d, want %d for %s", got, want, query)
	}
}

func orderInput(menuItemID string, quantity int) domain.CreateOrderInput {
	return domain.CreateOrderInput{
		RestaurantID: demoRestaurantID, CustomerName: "Test", CustomerPhone: "+79990000000",
		DeliveryAddress: "Test address",
		Items:           []domain.CreateOrderItem{{MenuItemID: menuItemID, Quantity: quantity}},
	}
}

func assertStock(t *testing.T, ctx context.Context, db *sql.DB, menuItemID string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx,
		"SELECT stock_quantity FROM menu_items WHERE id = $1", menuItemID,
	).Scan(&got); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if got != want {
		t.Fatalf("stock = %d, want %d", got, want)
	}
}

func deleteOrder(t *testing.T, db *sql.DB, orderID string) {
	t.Helper()
	if orderID == "" {
		return
	}
	if _, err := db.ExecContext(context.Background(), "DELETE FROM orders WHERE id = $1", orderID); err != nil {
		t.Logf("cleanup order %s: %v", orderID, err)
	}
}
