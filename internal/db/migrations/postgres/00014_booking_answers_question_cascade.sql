-- +goose Up
-- booking_answers.question_id referenced event_type_questions(id) with no
-- ON DELETE rule, so deleting an intake question that already had responses
-- (or deleting an event type that owns such questions) failed with a foreign-key
-- violation surfaced as a 500. The SQLite migration recreates the table because it
-- cannot alter a constraint; Postgres replaces the constraint in place.
--
-- booking_answers_question_id_fkey is the name Postgres gave the inline REFERENCES
-- in 00001: <table>_<column>_fkey. A Postgres install can only have reached this
-- version through that migration, so the name is not a guess.
ALTER TABLE booking_answers
    DROP CONSTRAINT booking_answers_question_id_fkey,
    ADD CONSTRAINT booking_answers_question_id_fkey
        FOREIGN KEY (question_id) REFERENCES event_type_questions(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE booking_answers
    DROP CONSTRAINT booking_answers_question_id_fkey,
    ADD CONSTRAINT booking_answers_question_id_fkey
        FOREIGN KEY (question_id) REFERENCES event_type_questions(id);
