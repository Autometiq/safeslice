-- Seed data for the end-to-end test.
--
-- The canary values exist so the test can prove masking happened. If any of
-- them survives into the target database, personal data reached a developer
-- laptop and the build must fail.

-- companies.owner_id is inserted NULL first: the users<->companies cycle means
-- neither table can be populated before the other.
INSERT INTO companies (name, slug) VALUES
    ('CanaryCorp Ltd', 'canary-slug-0001'),
    ('Beta Industries', 'beta-slug'),
    ('Gamma Holdings', 'gamma-slug');

INSERT INTO users (company_id, manager_id, email, first_name, last_name, password)
SELECT c.id, NULL,
       'canary' || g || '@real.example',
       'Zcanaryfirst' || g,
       'Zcanarylast' || g,
       'hunter2-canary-' || g
FROM generate_series(1, 12) AS g
JOIN companies c ON c.id = (g % 3) + 1;

-- Self-reference: every user after the first reports to user 1.
UPDATE users SET manager_id = (SELECT min(id) FROM users) WHERE id > (SELECT min(id) FROM users);

-- Close the cycle.
UPDATE companies SET owner_id = (SELECT min(u.id) FROM users u WHERE u.company_id = companies.id);

INSERT INTO events (user_id, created_at)
SELECT u.id, DATE '2026-03-01' + ((u.id % 20)::int) FROM users u;

INSERT INTO comments (commentable_type, commentable_id, body)
SELECT 'User', u.id, 'canary note from ' || u.email FROM users u;

INSERT INTO order_items (order_id, line_no, sku) VALUES (1, 1, 'SKU-1'), (1, 2, 'SKU-2');
INSERT INTO shipments (order_id, line_no) VALUES (1, 1);
