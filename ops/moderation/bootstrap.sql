-- Run ONCE with a database administrator, on the chosen application database.
-- psql reads passwords from the environment; never commit real credentials.
\set ON_ERROR_STOP on
\getenv moderation_owner_password MODERATION_OWNER_PASSWORD
\getenv moderation_runtime_password MODERATION_RUNTIME_PASSWORD
BEGIN;
CREATE ROLE moderation_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD :'moderation_owner_password';
CREATE ROLE moderation_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD :'moderation_runtime_password';
CREATE SCHEMA moderation AUTHORIZATION moderation_owner;
REVOKE ALL ON SCHEMA moderation FROM PUBLIC;
DO $$
BEGIN
    IF has_database_privilege('moderation_owner', current_database(), 'CREATE')
       OR has_database_privilege('moderation_runtime', current_database(), 'CREATE')
       OR has_schema_privilege('moderation_runtime', 'public', 'CREATE') THEN
        RAISE EXCEPTION 'Database/public schema grants CREATE to these roles; review PUBLIC privileges before provisioning Content';
    END IF;
END
$$;
COMMIT;
-- No grants on Identity or other schemas. The database must not grant CREATE
-- to PUBLIC and public schema must not grant CREATE to PUBLIC (PG18 defaults).
