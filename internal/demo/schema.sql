-- A small SaaS billing application.
--
-- Deliberately ordinary: this is roughly what a Rails or Django app looks like
-- after two years. It also happens to contain every shape that makes database
-- subsetting hard, which is the point -- a demo on a schema without those
-- proves nothing.

-- organizations <-> users is a cycle, and neither constraint is DEFERRABLE,
-- which is exactly what ORMs generate. SET CONSTRAINTS ALL DEFERRED does
-- nothing here.
CREATE TABLE organizations (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          varchar(120) NOT NULL,
    billing_email varchar(255),
    owner_id      bigint,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id              bigserial PRIMARY KEY,
    organization_id bigint NOT NULL REFERENCES organizations (id),
    manager_id      bigint REFERENCES users (id),          -- self-reference
    email           varchar(255) NOT NULL,
    first_name      text,
    last_name       text,
    -- Generated: Postgres recomputes it from the *masked* names on load.
    full_name       text GENERATED ALWAYS AS (first_name || ' ' || last_name) STORED,
    phone           varchar(32),
    password_hash   text,
    last_login_ip   inet,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- A unique index with no constraint behind it. Masking email without noticing
-- this produces duplicate-key failures on restore.
CREATE UNIQUE INDEX users_email_key ON users (email);

ALTER TABLE organizations
    ADD CONSTRAINT organizations_owner_fkey FOREIGN KEY (owner_id) REFERENCES users (id);

CREATE TABLE subscriptions (
    id              bigserial PRIMARY KEY,
    organization_id bigint NOT NULL REFERENCES organizations (id),
    plan            text NOT NULL,
    seats           int NOT NULL,
    renews_on       date NOT NULL
);

CREATE TABLE orders (
    id          bigserial PRIMARY KEY,
    user_id     bigint NOT NULL REFERENCES users (id),
    total_cents bigint NOT NULL,
    placed_at   timestamptz NOT NULL DEFAULT now()
);

-- Composite primary key, and a composite foreign key pointing at it.
CREATE TABLE order_lines (
    order_id bigint NOT NULL REFERENCES orders (id),
    line_no  int    NOT NULL,
    sku      text   NOT NULL,
    qty      int    NOT NULL,
    PRIMARY KEY (order_id, line_no)
);

CREATE TABLE shipments (
    id       bigserial PRIMARY KEY,
    order_id bigint NOT NULL,
    line_no  int    NOT NULL,
    carrier  text,
    FOREIGN KEY (order_id, line_no) REFERENCES order_lines (order_id, line_no)
);

CREATE TABLE payments (
    id              bigserial PRIMARY KEY,
    order_id        bigint NOT NULL REFERENCES orders (id),
    card_number     text,          -- a real card number in the source
    card_last4      varchar(4),
    billing_address text,
    paid_at         timestamptz NOT NULL DEFAULT now()
);

-- Polymorphic association, Rails style. There is no foreign key here for
-- safeslice to discover -- it has to be declared as a virtual key, or these
-- rows are silently left out of every slice.
CREATE TABLE notes (
    id          bigserial PRIMARY KEY,
    notable_type text   NOT NULL,   -- 'User' or 'Order'
    notable_id   bigint NOT NULL,
    body         text,              -- free text, and full of personal data
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Declarative partitioning, keyed on (id, occurred_on). A timestamp inside a
-- primary key is ordinary here, and it is the shape that breaks naive tools.
CREATE TABLE events (
    id          bigserial,
    user_id     bigint NOT NULL REFERENCES users (id),
    kind        text   NOT NULL,
    occurred_on date   NOT NULL,
    PRIMARY KEY (id, occurred_on)
) PARTITION BY RANGE (occurred_on);

CREATE TABLE events_2026_h1 PARTITION OF events
    FOR VALUES FROM ('2026-01-01') TO ('2026-07-01');
CREATE TABLE events_2026_h2 PARTITION OF events
    FOR VALUES FROM ('2026-07-01') TO ('2027-01-01');
