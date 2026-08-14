-- Every schema shape that breaks a naive subsetter, in one file.
-- Used by the catalog and graph tests.

CREATE TABLE companies (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,  -- rejects plain INSERT
    owner_id bigint,                                           -- cycle: -> users.id
    name     varchar(120) NOT NULL,
    slug     text NOT NULL,
    CONSTRAINT companies_slug_key UNIQUE (slug)
);

CREATE TABLE users (
    id         bigserial PRIMARY KEY,                          -- owned sequence
    company_id bigint NOT NULL REFERENCES companies (id),
    manager_id bigint REFERENCES users (id),                   -- self-reference
    email      varchar(255) NOT NULL,
    first_name text,
    last_name  text,
    full_name  text GENERATED ALWAYS AS (first_name || ' ' || last_name) STORED,
    password   text
);

-- Unique index with no backing constraint: still enforced, still a masking hazard.
CREATE UNIQUE INDEX users_email_uidx ON users (email);

-- Closes the cycle. Deferrable so the pair can be loaded in one transaction.
ALTER TABLE companies
    ADD CONSTRAINT companies_owner_fk FOREIGN KEY (owner_id)
    REFERENCES users (id) DEFERRABLE INITIALLY DEFERRED;

-- Composite key: column order in the constraint is significant.
CREATE TABLE order_items (
    order_id bigint NOT NULL,
    line_no  int    NOT NULL,
    sku      text   NOT NULL,
    PRIMARY KEY (order_id, line_no)
);

CREATE TABLE shipments (
    id       bigserial PRIMARY KEY,
    order_id bigint NOT NULL,
    line_no  int    NOT NULL,
    FOREIGN KEY (order_id, line_no) REFERENCES order_items (order_id, line_no)
);

-- Polymorphic association (Rails/Django style). No pg_constraint row exists,
-- so the graph cannot discover this without a virtual key in config.
CREATE TABLE comments (
    id               bigserial PRIMARY KEY,
    commentable_type text   NOT NULL,
    commentable_id   bigint NOT NULL,
    body             text
);

-- Declarative partitioning: reads must target the parent, not each partition.
CREATE TABLE events (
    id         bigserial,
    user_id    bigint NOT NULL REFERENCES users (id),
    created_at date   NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE events_2026 PARTITION OF events
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
