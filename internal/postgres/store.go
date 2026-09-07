package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/avito-intern/kitchen/internal/domain"
)

type Store struct {
	db *sql.DB
}

type OutboxEvent struct {
	ID             string
	DestinationURL string
	Payload        []byte
	Attempts       int
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) ListRestaurants(ctx context.Context) ([]domain.Restaurant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, address, status, created_at, updated_at
		FROM restaurants
		WHERE status = 'open'
		ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list restaurants: %w", err)
	}
	defer rows.Close()
	restaurants := make([]domain.Restaurant, 0)
	for rows.Next() {
		var restaurant domain.Restaurant
		if err := rows.Scan(
			&restaurant.ID, &restaurant.Name, &restaurant.Description, &restaurant.Address,
			&restaurant.Status, &restaurant.CreatedAt, &restaurant.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan restaurant: %w", err)
		}
		restaurants = append(restaurants, restaurant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate restaurants: %w", err)
	}
	return restaurants, nil
}

func (s *Store) GetMenu(ctx context.Context, restaurantID string) ([]domain.MenuItem, error) {
	var status string
	if err := s.db.QueryRowContext(ctx,
		"SELECT status FROM restaurants WHERE id = $1", restaurantID,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get restaurant for menu: %w", err)
	}
	if status != "open" {
		return nil, domain.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, restaurant_id, external_id, name, description, price_minor,
		       currency, stock_quantity, is_available, version, created_at, updated_at
		FROM menu_items
		WHERE restaurant_id = $1 AND is_available = true AND stock_quantity > 0
		ORDER BY name, id`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("get menu: %w", err)
	}
	defer rows.Close()
	items := make([]domain.MenuItem, 0)
	for rows.Next() {
		item, err := scanMenuItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan menu item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate menu items: %w", err)
	}
	return items, nil
}

func (s *Store) AuthenticatePartner(ctx context.Context, tokenHash string) (domain.Restaurant, error) {
	var restaurant domain.Restaurant
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, address, status, created_at, updated_at
		FROM restaurants WHERE partner_token_hash = $1`, tokenHash,
	).Scan(
		&restaurant.ID, &restaurant.Name, &restaurant.Description, &restaurant.Address,
		&restaurant.Status, &restaurant.CreatedAt, &restaurant.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Restaurant{}, domain.ErrForbidden
	}
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("authenticate partner: %w", err)
	}
	return restaurant, nil
}

func (s *Store) UpsertMenuItem(
	ctx context.Context,
	restaurantID, externalID string,
	input domain.UpsertMenuItemInput,
) (domain.MenuItem, error) {
	conflictClause := `DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			price_minor = EXCLUDED.price_minor,
			currency = EXCLUDED.currency,
			stock_quantity = EXCLUDED.stock_quantity,
			is_available = EXCLUDED.is_available,
			version = menu_items.version + 1,
			updated_at = now()`
	if input.CreateOnly {
		conflictClause = "DO NOTHING"
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO menu_items (
			restaurant_id, external_id, name, description, price_minor,
			currency, stock_quantity, is_available
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (restaurant_id, external_id) `+conflictClause+`
		RETURNING id, restaurant_id, external_id, name, description, price_minor,
		          currency, stock_quantity, is_available, version, created_at, updated_at`,
		restaurantID, externalID, input.Name, input.Description, input.PriceMinor,
		input.Currency, input.StockQuantity, input.IsAvailable,
	)
	item, err := scanMenuItem(row)
	if errors.Is(err, sql.ErrNoRows) && input.CreateOnly {
		return domain.MenuItem{}, domain.ErrAlreadyExists
	}
	if err != nil {
		return domain.MenuItem{}, fmt.Errorf("upsert menu item: %w", err)
	}
	return item, nil
}

func (s *Store) SetRestaurantStatus(ctx context.Context, restaurantID, status string) (domain.Restaurant, error) {
	var restaurant domain.Restaurant
	err := s.db.QueryRowContext(ctx, `
		UPDATE restaurants SET status = $2, updated_at = now()
		WHERE id = $1
		RETURNING id, name, description, address, status, created_at, updated_at`,
		restaurantID, status,
	).Scan(
		&restaurant.ID, &restaurant.Name, &restaurant.Description, &restaurant.Address,
		&restaurant.Status, &restaurant.CreatedAt, &restaurant.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Restaurant{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Restaurant{}, fmt.Errorf("set restaurant status: %w", err)
	}
	return restaurant, nil
}

func (s *Store) CreateOrder(
	ctx context.Context,
	userID, idempotencyKey, requestHash string,
	input domain.CreateOrderInput,
) (domain.Order, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("begin create order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A length prefix is unambiguous without inserting a NUL, which PostgreSQL
	// text values cannot contain. The namespace separates different lock uses.
	lockKey := fmt.Sprintf("create-order:%d:%s%s", len(userID), userID, idempotencyKey)
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", lockKey); err != nil {
		return domain.Order{}, false, fmt.Errorf("lock idempotency key: %w", err)
	}
	var existingID, existingHash string
	err = tx.QueryRowContext(ctx, `
		SELECT id, request_hash FROM orders
		WHERE avito_user_id = $1 AND idempotency_key = $2`, userID, idempotencyKey,
	).Scan(&existingID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return domain.Order{}, false, domain.ErrIdempotencyConflict
		}
		order, err := queryOrder(ctx, tx, existingID, false)
		return order, true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Order{}, false, fmt.Errorf("find idempotent order: %w", err)
	}

	var restaurantStatus, callbackURL string
	if err := tx.QueryRowContext(ctx, `
		SELECT status, callback_url FROM restaurants WHERE id = $1 FOR SHARE`, input.RestaurantID,
	).Scan(&restaurantStatus, &callbackURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Order{}, false, domain.ErrNotFound
		}
		return domain.Order{}, false, fmt.Errorf("lock restaurant: %w", err)
	}
	if restaurantStatus != "open" {
		return domain.Order{}, false, domain.ErrRestaurantClosed
	}

	order := domain.Order{
		ID:              uuid.NewString(),
		AvitoUserID:     userID,
		RestaurantID:    input.RestaurantID,
		Status:          domain.OrderPendingRestaurant,
		CustomerName:    input.CustomerName,
		CustomerPhone:   input.CustomerPhone,
		DeliveryAddress: input.DeliveryAddress,
		Currency:        "RUB",
		Items:           make([]domain.OrderItem, 0, len(input.Items)),
	}
	// Every path that locks multiple menu rows uses this same order, including
	// stock restoration on rejection/cancellation, to avoid deadlocks.
	requestedItems := append([]domain.CreateOrderItem(nil), input.Items...)
	sort.Slice(requestedItems, func(i, j int) bool {
		return requestedItems[i].MenuItemID < requestedItems[j].MenuItemID
	})
	for _, requested := range requestedItems {
		var item domain.MenuItem
		err := tx.QueryRowContext(ctx, `
			SELECT id, restaurant_id, external_id, name, description, price_minor,
			       currency, stock_quantity, is_available, version, created_at, updated_at
			FROM menu_items WHERE id = $1 FOR UPDATE`, requested.MenuItemID,
		).Scan(
			&item.ID, &item.RestaurantID, &item.ExternalID, &item.Name, &item.Description,
			&item.PriceMinor, &item.Currency, &item.StockQuantity, &item.IsAvailable,
			&item.Version, &item.CreatedAt, &item.UpdatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Order{}, false, fmt.Errorf("%w: menu item %s", domain.ErrNotFound, requested.MenuItemID)
		}
		if err != nil {
			return domain.Order{}, false, fmt.Errorf("lock menu item: %w", err)
		}
		if item.RestaurantID != input.RestaurantID {
			return domain.Order{}, false, fmt.Errorf("%w: menu item %s", domain.ErrNotFound, requested.MenuItemID)
		}
		if !item.IsAvailable || item.StockQuantity < requested.Quantity {
			return domain.Order{}, false, fmt.Errorf("%w: menu item %s", domain.ErrInsufficientStock, requested.MenuItemID)
		}
		subtotal := item.PriceMinor * int64(requested.Quantity)
		order.TotalPriceMinor += subtotal
		order.Items = append(order.Items, domain.OrderItem{
			MenuItemID: item.ID, ExternalID: item.ExternalID, Name: item.Name,
			UnitPriceMinor: item.PriceMinor, Quantity: requested.Quantity, SubtotalMinor: subtotal,
		})
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO orders (
			id, avito_user_id, restaurant_id, idempotency_key, request_hash, status,
			customer_name, customer_phone, delivery_address, total_price_minor, currency
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at, updated_at`,
		order.ID, userID, input.RestaurantID, idempotencyKey, requestHash, order.Status,
		order.CustomerName, order.CustomerPhone, order.DeliveryAddress,
		order.TotalPriceMinor, order.Currency,
	).Scan(&order.CreatedAt, &order.UpdatedAt)
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("insert order: %w", err)
	}
	for _, item := range order.Items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO order_items (
				order_id, menu_item_id, external_id_snapshot, name_snapshot,
				unit_price_minor, quantity, subtotal_minor
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			order.ID, item.MenuItemID, item.ExternalID, item.Name,
			item.UnitPriceMinor, item.Quantity, item.SubtotalMinor,
		); err != nil {
			return domain.Order{}, false, fmt.Errorf("insert order item: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE menu_items
			SET stock_quantity = stock_quantity - $2, version = version + 1, updated_at = now()
			WHERE id = $1`, item.MenuItemID, item.Quantity,
		); err != nil {
			return domain.Order{}, false, fmt.Errorf("reserve stock: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_status_events(event_id, order_id, actor, from_status, to_status)
		VALUES ($1, $2, 'system', NULL, $3)`,
		uuid.NewString(), order.ID, order.Status,
	); err != nil {
		return domain.Order{}, false, fmt.Errorf("insert initial status event: %w", err)
	}
	if err := insertOutbox(ctx, tx, callbackURL, "order.created", 1, order); err != nil {
		return domain.Order{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Order{}, false, fmt.Errorf("commit create order: %w", err)
	}
	return order, false, nil
}

func (s *Store) GetOrder(ctx context.Context, orderID string) (domain.Order, error) {
	return queryOrder(ctx, s.db, orderID, false)
}

func (s *Store) GetPartnerOrder(ctx context.Context, restaurantID, orderID string) (domain.Order, error) {
	order, err := queryOrder(ctx, s.db, orderID, false)
	if err != nil {
		return domain.Order{}, err
	}
	if order.RestaurantID != restaurantID {
		return domain.Order{}, domain.ErrNotFound
	}
	return order, nil
}

func (s *Store) CancelOrder(ctx context.Context, userID, orderID, eventID string) (domain.Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Order{}, fmt.Errorf("begin cancel order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	order, err := queryOrder(ctx, tx, orderID, true)
	if err != nil {
		return domain.Order{}, err
	}
	if order.AvitoUserID != userID {
		return domain.Order{}, domain.ErrNotFound
	}
	if !domain.CanTransition(order.Status, domain.OrderCancelled) {
		return domain.Order{}, domain.ErrInvalidTransition
	}
	previous := order.Status
	order.Status = domain.OrderCancelled
	if err := tx.QueryRowContext(ctx,
		"UPDATE orders SET status = $2, updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond') WHERE id = $1 RETURNING updated_at", order.ID, order.Status,
	).Scan(&order.UpdatedAt); err != nil {
		return domain.Order{}, fmt.Errorf("cancel order: %w", err)
	}
	if err := restoreStock(ctx, tx, order.ID); err != nil {
		return domain.Order{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_status_events(event_id, order_id, actor, from_status, to_status)
		VALUES ($1, $2, 'user', $3, $4)`, eventID, order.ID, previous, order.Status,
	); err != nil {
		return domain.Order{}, fmt.Errorf("record cancellation: %w", err)
	}
	var callbackURL string
	if err := tx.QueryRowContext(ctx,
		"SELECT callback_url FROM restaurants WHERE id = $1", order.RestaurantID,
	).Scan(&callbackURL); err != nil {
		return domain.Order{}, fmt.Errorf("get cancellation callback: %w", err)
	}
	if err := insertOutbox(ctx, tx, callbackURL, "order.cancelled", 2, order); err != nil {
		return domain.Order{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Order{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return order, nil
}

func (s *Store) UpdateOrderStatus(
	ctx context.Context,
	restaurantID, orderID, eventID string,
	newStatus domain.OrderStatus,
) (domain.Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Order{}, fmt.Errorf("begin status update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize concurrent retries of the same partner event. Without this lock,
	// both transactions could observe a missing event before one of them commits.
	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "partner-event:"+eventID,
	); err != nil {
		return domain.Order{}, fmt.Errorf("lock status event: %w", err)
	}
	// Ownership is checked even for a replay: event IDs are not credentials.
	order, err := queryOrder(ctx, tx, orderID, true)
	if err != nil {
		return domain.Order{}, err
	}
	if order.RestaurantID != restaurantID {
		return domain.Order{}, domain.ErrNotFound
	}
	var savedOrderID string
	var savedStatus domain.OrderStatus
	var savedActor string
	err = tx.QueryRowContext(ctx,
		"SELECT order_id, to_status, actor FROM order_status_events WHERE event_id = $1", eventID,
	).Scan(&savedOrderID, &savedStatus, &savedActor)
	if err == nil {
		if savedOrderID != orderID || savedStatus != newStatus || savedActor != "restaurant" {
			return domain.Order{}, domain.ErrIdempotencyConflict
		}
		return order, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("check status event: %w", err)
	}
	if !domain.CanTransition(order.Status, newStatus) {
		return domain.Order{}, domain.ErrInvalidTransition
	}
	previous := order.Status
	order.Status = newStatus
	if err := tx.QueryRowContext(ctx,
		"UPDATE orders SET status = $2, updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond') WHERE id = $1 RETURNING updated_at", order.ID, order.Status,
	).Scan(&order.UpdatedAt); err != nil {
		return domain.Order{}, fmt.Errorf("update order status: %w", err)
	}
	if newStatus == domain.OrderRejected {
		if err := restoreStock(ctx, tx, order.ID); err != nil {
			return domain.Order{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_status_events(event_id, order_id, actor, from_status, to_status)
		VALUES ($1, $2, 'restaurant', $3, $4)`, eventID, order.ID, previous, newStatus,
	); err != nil {
		return domain.Order{}, fmt.Errorf("record order status: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Order{}, fmt.Errorf("commit status update: %w", err)
	}
	return order, nil
}

func (s *Store) ClaimOutbox(ctx context.Context) (OutboxEvent, bool, error) {
	var event OutboxEvent
	err := s.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT e.id
			FROM outbox_events e
			WHERE (
				(e.status = 'pending' AND e.available_at <= now())
				OR (e.status = 'processing' AND e.locked_at < now() - interval '1 minute')
			)
			AND NOT EXISTS (
				SELECT 1 FROM outbox_events earlier
				WHERE earlier.aggregate_id = e.aggregate_id
				  AND earlier.sequence_no < e.sequence_no
				  AND earlier.status IN ('pending', 'processing')
			)
			ORDER BY e.available_at, e.created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE outbox_events e
		SET status = 'processing', locked_at = now(), attempts = attempts + 1
		FROM candidate
		WHERE e.id = candidate.id
		RETURNING e.id, e.destination_url, e.payload, e.attempts`,
	).Scan(&event.ID, &event.DestinationURL, &event.Payload, &event.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxEvent{}, false, nil
	}
	if err != nil {
		return OutboxEvent{}, false, fmt.Errorf("claim outbox event: %w", err)
	}
	return event, true, nil
}

func (s *Store) MarkOutboxDelivered(ctx context.Context, eventID string, attempt int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'delivered', delivered_at = now(), locked_at = NULL, last_error = NULL
		WHERE id = $1 AND status = 'processing' AND attempts = $2`, eventID, attempt)
	if err != nil {
		return fmt.Errorf("mark outbox delivered: %w", err)
	}
	return nil
}

func (s *Store) MarkOutboxFailed(ctx context.Context, eventID string, attempt int, message string, retryAt time.Time) error {
	// Remote error responses may contain invalid UTF-8 or NUL bytes. Keep the
	// diagnostic text valid for PostgreSQL even for an arbitrary callback body.
	message = strings.ReplaceAll(strings.ToValidUTF8(message, "?"), "\x00", "?")
	if runes := []rune(message); len(runes) > 1000 {
		message = string(runes[:1000])
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'pending', available_at = $3, locked_at = NULL, last_error = $4
		WHERE id = $1 AND status = 'processing' AND attempts = $2`, eventID, attempt, retryAt, message)
	if err != nil {
		return fmt.Errorf("reschedule outbox event: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(...any) error
}

func scanMenuItem(row scanner) (domain.MenuItem, error) {
	var item domain.MenuItem
	err := row.Scan(
		&item.ID, &item.RestaurantID, &item.ExternalID, &item.Name, &item.Description,
		&item.PriceMinor, &item.Currency, &item.StockQuantity, &item.IsAvailable,
		&item.Version, &item.CreatedAt, &item.UpdatedAt,
	)
	return item, err
}

func queryOrder(ctx context.Context, q queryer, orderID string, forUpdate bool) (domain.Order, error) {
	query := `
		SELECT id, avito_user_id, restaurant_id, status, customer_name, customer_phone,
		       delivery_address, total_price_minor, currency, created_at, updated_at
		FROM orders WHERE id = $1`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var order domain.Order
	err := q.QueryRowContext(ctx, query, orderID).Scan(
		&order.ID, &order.AvitoUserID, &order.RestaurantID, &order.Status,
		&order.CustomerName, &order.CustomerPhone, &order.DeliveryAddress,
		&order.TotalPriceMinor, &order.Currency, &order.CreatedAt, &order.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Order{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("get order: %w", err)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT menu_item_id, external_id_snapshot, name_snapshot,
		       unit_price_minor, quantity, subtotal_minor
		FROM order_items WHERE order_id = $1 ORDER BY menu_item_id`, orderID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("get order items: %w", err)
	}
	defer rows.Close()
	order.Items = make([]domain.OrderItem, 0)
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(
			&item.MenuItemID, &item.ExternalID, &item.Name,
			&item.UnitPriceMinor, &item.Quantity, &item.SubtotalMinor,
		); err != nil {
			return domain.Order{}, fmt.Errorf("scan order item: %w", err)
		}
		order.Items = append(order.Items, item)
	}
	if err := rows.Err(); err != nil {
		return domain.Order{}, fmt.Errorf("iterate order items: %w", err)
	}
	return order, nil
}

func restoreStock(ctx context.Context, tx *sql.Tx, orderID string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT menu.id FROM menu_items AS menu
		JOIN order_items AS items ON items.menu_item_id = menu.id
		WHERE items.order_id = $1
		ORDER BY menu.id
		FOR UPDATE OF menu`, orderID)
	if err != nil {
		return fmt.Errorf("lock stock for restoration: %w", err)
	}
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read stock restoration lock: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate stock restoration locks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close stock restoration locks: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE menu_items AS menu
		SET stock_quantity = menu.stock_quantity + items.quantity,
		    version = menu.version + 1,
		    updated_at = now()
		FROM order_items AS items
		WHERE items.order_id = $1 AND items.menu_item_id = menu.id`, orderID)
	if err != nil {
		return fmt.Errorf("restore stock: %w", err)
	}
	return nil
}

func insertOutbox(
	ctx context.Context,
	tx *sql.Tx,
	destinationURL, eventType string,
	sequence int,
	order domain.Order,
) error {
	eventID := uuid.NewString()
	payload, err := json.Marshal(struct {
		EventID    string       `json:"event_id"`
		EventType  string       `json:"event_type"`
		OccurredAt time.Time    `json:"occurred_at"`
		Order      domain.Order `json:"order"`
	}{
		EventID: eventID, EventType: eventType, OccurredAt: time.Now().UTC(), Order: order,
	})
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_events (
			id, aggregate_id, sequence_no, event_type, destination_url, payload
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		eventID, order.ID, sequence, eventType, destinationURL, string(payload),
	); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}
