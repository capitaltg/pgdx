-- Demo schema for the pgdx shell recording (demo/shell.tape).
-- A small, friendly e-commerce model: customers, products, orders, order_items.
-- Seeded with enough rows that sizes and row estimates look real on screen.

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    full_name  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE products (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sku      text NOT NULL UNIQUE,
    name     text NOT NULL,
    price    numeric(10,2) NOT NULL,
    in_stock boolean NOT NULL DEFAULT true
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    status      text NOT NULL DEFAULT 'pending',
    total       numeric(12,2) NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_customer_id_idx ON orders (customer_id);
CREATE INDEX orders_created_at_idx ON orders (created_at);

CREATE TABLE order_items (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id   bigint NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id bigint NOT NULL REFERENCES products(id),
    quantity   integer NOT NULL DEFAULT 1,
    unit_price numeric(10,2) NOT NULL
);
CREATE INDEX order_items_order_id_idx ON order_items (order_id);

INSERT INTO customers (email, full_name)
SELECT 'user' || g || '@example.com', 'Customer ' || g
FROM generate_series(1, 500) g;

INSERT INTO products (sku, name, price)
SELECT 'SKU-' || lpad(g::text, 5, '0'), 'Product ' || g, (random() * 100)::numeric(10, 2)
FROM generate_series(1, 200) g;

INSERT INTO orders (customer_id, status, total)
SELECT (random() * 499 + 1)::int,
       (ARRAY['pending', 'paid', 'shipped', 'cancelled'])[ceil(random() * 4)],
       (random() * 500)::numeric(12, 2)
FROM generate_series(1, 2000) g;

INSERT INTO order_items (order_id, product_id, quantity, unit_price)
SELECT (random() * 1999 + 1)::int, (random() * 199 + 1)::int, ceil(random() * 5)::int, (random() * 100)::numeric(10, 2)
FROM generate_series(1, 6000) g;

ANALYZE;
