CREATE TABLE restaurants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    address text NOT NULL,
    status text NOT NULL CHECK (status IN ('open', 'closed')),
    callback_url text NOT NULL,
    partner_token_hash char(64) NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE menu_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id),
    external_id text NOT NULL,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    price_minor bigint NOT NULL CHECK (price_minor > 0),
    currency char(3) NOT NULL DEFAULT 'RUB',
    stock_quantity integer NOT NULL CHECK (stock_quantity >= 0),
    is_available boolean NOT NULL DEFAULT true,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (restaurant_id, external_id)
);

CREATE INDEX menu_items_public_idx
    ON menu_items (restaurant_id, is_available)
    WHERE is_available = true;

CREATE TABLE orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    avito_user_id text NOT NULL,
    restaurant_id uuid NOT NULL REFERENCES restaurants(id),
    idempotency_key text NOT NULL,
    request_hash char(64) NOT NULL,
    status text NOT NULL CHECK (status IN (
        'pending_restaurant', 'accepted', 'rejected', 'preparing',
        'ready', 'completed', 'cancelled'
    )),
    customer_name text NOT NULL,
    customer_phone text NOT NULL,
    delivery_address text NOT NULL,
    total_price_minor bigint NOT NULL CHECK (total_price_minor >= 0),
    currency char(3) NOT NULL DEFAULT 'RUB',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (avito_user_id, idempotency_key)
);

CREATE INDEX orders_user_created_idx ON orders (avito_user_id, created_at DESC);
CREATE INDEX orders_restaurant_created_idx ON orders (restaurant_id, created_at DESC);

CREATE TABLE order_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    menu_item_id uuid NOT NULL REFERENCES menu_items(id),
    external_id_snapshot text NOT NULL,
    name_snapshot text NOT NULL,
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor > 0),
    quantity integer NOT NULL CHECK (quantity > 0),
    subtotal_minor bigint NOT NULL CHECK (subtotal_minor > 0)
);

CREATE INDEX order_items_order_idx ON order_items (order_id);

CREATE TABLE order_status_events (
    event_id uuid PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    actor text NOT NULL CHECK (actor IN ('user', 'restaurant', 'system')),
    from_status text,
    to_status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX order_status_events_order_idx
    ON order_status_events (order_id, created_at);

CREATE TABLE outbox_events (
    id uuid PRIMARY KEY,
    aggregate_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    sequence_no integer NOT NULL CHECK (sequence_no > 0),
    event_type text NOT NULL,
    destination_url text NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'delivered', 'cancelled')),
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    locked_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    UNIQUE (aggregate_id, sequence_no)
);

CREATE INDEX outbox_pending_idx ON outbox_events (available_at, created_at)
    WHERE status = 'pending';

INSERT INTO restaurants (
    id, name, description, address, status, callback_url, partner_token_hash
) VALUES (
    '00000000-0000-0000-0000-000000000001',
    'Тесто & Томаты',
    'Демонстрационная пиццерия Авито.Кухни',
    'Москва, ул. Примерная, 1',
    'open',
    'http://demo-restaurant:8081/integration/v1/events',
    'aa78c3cd32f78e4d300117d81e20f1849a08a67c0da3bf1a3647fe4d3748caff'
);

-- Stable IDs make the documented end-to-end example reproducible. The demo
-- restaurant upserts these rows on startup, just as a real partner would.
INSERT INTO menu_items (
    id, restaurant_id, external_id, name, description,
    price_minor, currency, stock_quantity, is_available
) VALUES
    (
        '10000000-0000-0000-0000-000000000001',
        '00000000-0000-0000-0000-000000000001',
        'margherita', 'Пицца Маргарита', 'Томаты, моцарелла, базилик',
        59900, 'RUB', 20, true
    ),
    (
        '10000000-0000-0000-0000-000000000002',
        '00000000-0000-0000-0000-000000000001',
        'pepperoni', 'Пицца Пепперони', 'Пепперони, моцарелла, томатный соус',
        74900, 'RUB', 15, true
    ),
    (
        '10000000-0000-0000-0000-000000000003',
        '00000000-0000-0000-0000-000000000001',
        'lemonade', 'Домашний лимонад', 'Лимон, мята, содовая',
        24900, 'RUB', 30, true
    );
