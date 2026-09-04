-- +goose Up
-- Record what was actually charged on the booking (immutable payment record), independent
-- of the event type's current price_cents which can change later. Set from the Stripe
-- Checkout session's amount_total/currency at confirmation. 0/'' for free bookings.
ALTER TABLE bookings ADD COLUMN amount_paid_cents    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN amount_paid_currency TEXT    NOT NULL DEFAULT '';

-- +goose Down
-- Deliberately a no-op, mirroring the SQLite migration: Postgres could drop the
-- column, but a down that lands on a different schema per engine is worse than a
-- down that lands on none.
