-- Shared guard used by audit tables: history is append-only and must never be
-- rewritten or erased, not even by a buggy release or a manual query.
CREATE FUNCTION forbid_audit_modification() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'table % is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
