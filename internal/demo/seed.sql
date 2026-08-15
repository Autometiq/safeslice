-- Fabricated customers that look real enough to be dangerous.
--
-- Nothing here belongs to a real person, but every value is shaped like
-- production data: routable email domains, valid international phone numbers,
-- Luhn-valid card numbers, and personal details buried in free text. That is
-- what makes the demo worth running -- `safeslice verify` finds all of it in
-- the source, and none of it in the slice.

INSERT INTO organizations (name, billing_email)
SELECT 'Org ' || g, 'billing' || g || '@realcompany.com'
FROM generate_series(1, 120) g;

INSERT INTO users (organization_id, email, first_name, last_name, phone, password_hash, last_login_ip)
SELECT (g % 120) + 1,
       'person' || g || '@realcompany.com',
       (ARRAY['Priya','Marcus','Wei','Fatima','Diego','Anya','Kwame','Sofia'])[1 + g % 8],
       (ARRAY['Sharma','Johnson','Chen','Al-Amin','Rossi','Novak','Mensah','Garcia'])[1 + g % 8],
       '+44 7700 9' || lpad((g % 100000)::text, 5, '0'),
       'bcrypt$2y$10$' || md5(g::text),
       ('203.0.114.' || (g % 254 + 1))::inet
FROM generate_series(1, 4000) g;

-- Self-reference: everyone after the first fifty reports to one of them.
UPDATE users SET manager_id = ((id % 50) + 1) WHERE id > 50;

-- Close the organizations <-> users cycle.
UPDATE organizations
SET owner_id = (SELECT min(u.id) FROM users u WHERE u.organization_id = organizations.id);

INSERT INTO subscriptions (organization_id, plan, seats, renews_on)
SELECT o.id,
       (ARRAY['starter','team','enterprise'])[1 + o.id % 3],
       10 + (o.id % 90),
       DATE '2026-09-01' + ((o.id % 300)::int)
FROM organizations o;

INSERT INTO orders (user_id, total_cents)
SELECT u.id, 1000 + (u.id * 37) % 250000
FROM users u, generate_series(1, 2);

INSERT INTO order_lines (order_id, line_no, sku, qty)
SELECT o.id, l, 'SKU-' || (o.id % 500), 1 + (o.id % 4)
FROM orders o, generate_series(1, 2) l;

INSERT INTO shipments (order_id, line_no, carrier)
SELECT ol.order_id, ol.line_no, (ARRAY['dhl','ups','royal-mail'])[1 + ol.order_id % 3]
FROM order_lines ol WHERE ol.line_no = 1;

-- 4111111111111111 is the standard Visa test number: Luhn-valid, so the card
-- detector in `safeslice verify` recognises it as a card rather than an id.
INSERT INTO payments (order_id, card_number, card_last4, billing_address)
SELECT o.id,
       '4111111111111111',
       '1111',
       (100 + o.id % 900) || ' Kingsway, London, WC2B 6' || chr((65 + o.id % 26)::int)
FROM orders o;

-- Free text is where personal data hides, and no column-name heuristic can
-- find it. This is the column the demo asks you to classify by hand.
INSERT INTO notes (notable_type, notable_id, body)
SELECT 'User', u.id,
       'Spoke to ' || u.first_name || ' ' || u.last_name || ' on ' || u.phone ||
       '. Confirmed billing to ' || u.email || ', card ending 1111.'
FROM users u WHERE u.id % 3 = 0;

INSERT INTO events (user_id, kind, occurred_on)
SELECT u.id,
       (ARRAY['login','export','invite','billing_view'])[1 + u.id % 4],
       DATE '2026-01-01' + (((u.id * 7) % 360)::int)
FROM users u, generate_series(1, 3);
