package notty

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	crdt "notty/internal/ycrdt"
)

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

const postgresCheckpointInterval = 100
const postgresCheckpointTailLimit = postgresCheckpointInterval

func initPostgresSchemaTables(db *sql.DB) error {
	statements := []string{
		`
		CREATE TABLE IF NOT EXISTS workspaces (
			id UUID PRIMARY KEY,
			slug TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			root_document_id UUID NOT NULL,
			default_runtime TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		// Idempotent column-add for databases created before default_runtime existed
		// (instant backfill: the column carries a default).
		`ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS default_runtime TEXT NOT NULL DEFAULT ''`,
		`
		CREATE TABLE IF NOT EXISTS accounts (
			id UUID PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			email_verified BOOLEAN NOT NULL DEFAULT FALSE,
			last_accessed_workspace_id UUID,
			password_updated_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_accounts_last_workspace
				FOREIGN KEY (last_accessed_workspace_id)
				REFERENCES workspaces(id)
				ON DELETE SET NULL
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS account_email_tokens (
			id UUID PRIMARY KEY,
			account_id UUID NOT NULL,
			purpose TEXT NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_account_email_tokens_account
				FOREIGN KEY (account_id)
				REFERENCES accounts(id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_account_email_tokens_account_purpose_created ON account_email_tokens (account_id, purpose, created_at DESC)`,
		`
		CREATE TABLE IF NOT EXISTS users (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			handle TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_users_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_users_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE
		)
		`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_workspace_handle ON users (workspace_id, handle)`,
		`
		CREATE TABLE IF NOT EXISTS daemons (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'active',
			daemon_version TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			runtime_detections JSONB NOT NULL DEFAULT '[]'::jsonb,
			last_seen_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ,
			CONSTRAINT uq_daemons_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_daemons_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_daemons_workspace ON daemons (workspace_id, status)`,
		`
		CREATE TABLE IF NOT EXISTS documents (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			hidden BOOLEAN NOT NULL DEFAULT false,
			client_id_seed BIGINT NOT NULL,
			create_client_operation_id TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_documents_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_documents_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS document_heads (
			workspace_id UUID NOT NULL,
			document_id UUID PRIMARY KEY,
			state_vector TEXT NOT NULL DEFAULT '',
			update_id BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_document_heads_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_document_heads_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS document_updates (
			id BIGSERIAL PRIMARY KEY,
			workspace_id UUID NOT NULL,
			document_id UUID NOT NULL,
			update BYTEA NOT NULL,
			actor_id UUID,
			actor_type TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_document_updates_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_document_updates_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_created ON document_updates (workspace_id, document_id, created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_id ON document_updates (workspace_id, document_id, id ASC)`,
		`
		CREATE TABLE IF NOT EXISTS document_checkpoints (
			id BIGSERIAL PRIMARY KEY,
			workspace_id UUID NOT NULL,
			document_id UUID NOT NULL,
			update_id BIGINT NOT NULL,
			crdt_state TEXT NOT NULL,
			state_vector TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (workspace_id, document_id, update_id),
			CONSTRAINT fk_document_checkpoints_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_document_checkpoints_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_document_checkpoints_workspace_document_update ON document_checkpoints (workspace_id, document_id, update_id DESC)`,
		`
		CREATE TABLE IF NOT EXISTS agents (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			daemon_id UUID,
			handle TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			kind TEXT NOT NULL,
			system_prompt TEXT NOT NULL,
			workspace_root TEXT NOT NULL,
			current_turn_id TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			current_task TEXT NOT NULL,
			current_activity TEXT NOT NULL,
			current_run_id UUID,
			last_heartbeat_at TIMESTAMPTZ,
			last_run_completed TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_agents_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_agents_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agents_daemon
				FOREIGN KEY (workspace_id, daemon_id)
				REFERENCES daemons(workspace_id, id)
				ON DELETE SET NULL (daemon_id)
		)
		`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_workspace_handle ON agents (workspace_id, handle)`,
		`
		CREATE TABLE IF NOT EXISTS agent_runs (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			agent_id UUID NOT NULL,
			agent_handle TEXT NOT NULL,
			agent_name TEXT NOT NULL,
			agent_kind TEXT NOT NULL,
			system_prompt TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			workspace_root TEXT NOT NULL,
			working_dir TEXT NOT NULL,
			prompt TEXT NOT NULL,
			status TEXT NOT NULL,
			desired_status TEXT NOT NULL,
			process_id INTEGER,
			launch_time TIMESTAMPTZ,
			last_heartbeat_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			exit_code INTEGER,
			last_message TEXT NOT NULL,
			log_tail JSONB NOT NULL DEFAULT '[]'::jsonb,
			error TEXT NOT NULL,
			assigned_task_ref TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_agent_runs_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_agent_runs_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agent_runs_agent
				FOREIGN KEY (workspace_id, agent_id)
				REFERENCES agents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_workspace_agent_updated ON agent_runs (workspace_id, agent_id, updated_at DESC)`,
		`
		CREATE TABLE IF NOT EXISTS workspace_members (
			workspace_id UUID NOT NULL,
			account_id UUID NOT NULL,
			user_id UUID NOT NULL,
			membership_role TEXT NOT NULL DEFAULT 'member',
			status TEXT NOT NULL DEFAULT 'active',
			invited_by UUID,
			last_accessed_document_id UUID,
			created_at TIMESTAMPTZ NOT NULL,
			accepted_at TIMESTAMPTZ,
			PRIMARY KEY (workspace_id, account_id),
			CONSTRAINT fk_workspace_members_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_workspace_members_account
				FOREIGN KEY (account_id)
				REFERENCES accounts(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_workspace_members_user
				FOREIGN KEY (workspace_id, user_id)
				REFERENCES users(workspace_id, id)
				ON DELETE CASCADE,
			CONSTRAINT fk_workspace_members_invited_by
				FOREIGN KEY (workspace_id, invited_by)
				REFERENCES users(workspace_id, id)
				ON DELETE SET NULL (invited_by),
			CONSTRAINT fk_workspace_members_last_doc
				FOREIGN KEY (workspace_id, last_accessed_document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE SET NULL (last_accessed_document_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_members_account ON workspace_members (account_id, status, workspace_id)`,
		`
		CREATE TABLE IF NOT EXISTS workspace_invites (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			created_by_user_id UUID NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_workspace_invites_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_workspace_invites_created_by
				FOREIGN KEY (workspace_id, created_by_user_id)
				REFERENCES users(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_invites_workspace ON workspace_invites (workspace_id)`,
		`
		CREATE TABLE IF NOT EXISTS threads (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			document_id UUID NOT NULL,
			client_operation_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			anchor_relative_start TEXT NOT NULL DEFAULT '',
			anchor_relative_end TEXT NOT NULL DEFAULT '',
			anchor_kind TEXT NOT NULL DEFAULT '',
			anchor_excerpt TEXT NOT NULL DEFAULT '',
			created_by_id UUID,
			created_by_type TEXT NOT NULL,
			created_by_handle TEXT NOT NULL,
			created_by_name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_threads_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_threads_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_threads_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_threads_workspace_document_updated ON threads (workspace_id, document_id, updated_at DESC)`,
		// Temporary scaffolding: replaces the old non-unique index with a unique
		// partial index for atomic thread-create idempotency via ON CONFLICT.
		// Teardown: remove the DROP once all environments carry the unique index.
		`DROP INDEX IF EXISTS idx_threads_workspace_actor_operation`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_threads_workspace_actor_operation ON threads (workspace_id, created_by_id, created_by_type, client_operation_id) WHERE client_operation_id <> ''`,
		`
		CREATE TABLE IF NOT EXISTS thread_messages (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			thread_id UUID NOT NULL,
			author_id UUID,
			author_type TEXT NOT NULL,
			author_handle TEXT NOT NULL,
			author_name TEXT NOT NULL,
			body TEXT NOT NULL,
			kind TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_thread_messages_workspace_id UNIQUE (workspace_id, id),
			CONSTRAINT fk_thread_messages_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_thread_messages_thread
				FOREIGN KEY (workspace_id, thread_id)
				REFERENCES threads(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_thread_messages_workspace_thread_created ON thread_messages (workspace_id, thread_id, created_at ASC)`,
		`
		CREATE TABLE IF NOT EXISTS thread_participants (
			workspace_id UUID NOT NULL,
			thread_id UUID NOT NULL,
			participant_id UUID NOT NULL,
			PRIMARY KEY (workspace_id, thread_id, participant_id),
			CONSTRAINT fk_thread_participants_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_thread_participants_thread
				FOREIGN KEY (workspace_id, thread_id)
				REFERENCES threads(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS presences (
			workspace_id UUID NOT NULL,
			actor_id UUID NOT NULL,
			actor_type TEXT NOT NULL,
			document_id UUID,
			file_path TEXT NOT NULL,
			mode TEXT NOT NULL,
			selection_start INTEGER,
			selection_end INTEGER,
			activity TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, actor_id),
			CONSTRAINT fk_presences_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_presences_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS activities (
			id BIGSERIAL PRIMARY KEY,
			workspace_id UUID NOT NULL,
			type TEXT NOT NULL,
			document_id UUID,
			actor_id UUID,
			actor_type TEXT NOT NULL,
			summary TEXT NOT NULL,
			occurred_at TIMESTAMPTZ NOT NULL,
			provenance_actor_id UUID,
			provenance_actor_type TEXT NOT NULL,
			provenance_execution_id TEXT NOT NULL,
			provenance_tool TEXT NOT NULL,
			provenance_trigger TEXT NOT NULL,
			provenance_autonomous BOOLEAN NOT NULL,
			provenance_confidence TEXT NOT NULL,
			provenance_requested_by TEXT NOT NULL,
			provenance_source TEXT NOT NULL,
			provenance_intended_scope TEXT NOT NULL,
			provenance_read_set_summary TEXT NOT NULL,
			presence_ref TEXT NOT NULL,
			CONSTRAINT fk_activities_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_activities_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE SET NULL (document_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_activities_workspace_occurred ON activities (workspace_id, occurred_at DESC, id DESC)`,
		`
		CREATE TABLE IF NOT EXISTS agent_events (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			agent_id UUID NOT NULL,
			agent_handle TEXT NOT NULL,
			type TEXT NOT NULL,
			box TEXT NOT NULL DEFAULT 'for_me',
			status TEXT NOT NULL,
			document_id UUID,
			thread_id UUID,
			thread_message_id UUID,
			from_update_id BIGINT NOT NULL DEFAULT 0,
			to_update_id BIGINT NOT NULL DEFAULT 0,
			summary TEXT NOT NULL,
			prompt TEXT NOT NULL,
			dedup_key TEXT NOT NULL,
			claimed_by UUID,
			run_id UUID,
			last_error TEXT NOT NULL,
			attempt_count INTEGER NOT NULL,
			available_at TIMESTAMPTZ NOT NULL,
			claimed_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT fk_agent_events_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agent_events_agent
				FOREIGN KEY (workspace_id, agent_id)
				REFERENCES agents(workspace_id, id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agent_events_run
				FOREIGN KEY (workspace_id, run_id)
				REFERENCES agent_runs(workspace_id, id)
				ON DELETE SET NULL (run_id),
			CONSTRAINT fk_agent_events_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE SET NULL (document_id),
			CONSTRAINT fk_agent_events_thread
				FOREIGN KEY (workspace_id, thread_id)
				REFERENCES threads(workspace_id, id)
				ON DELETE SET NULL (thread_id),
			CONSTRAINT fk_agent_events_thread_message
				FOREIGN KEY (workspace_id, thread_message_id)
				REFERENCES thread_messages(workspace_id, id)
				ON DELETE SET NULL (thread_message_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_agent_events_workspace_agent_claim ON agent_events (workspace_id, agent_id, status, available_at, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_events_workspace_agent_box_status ON agent_events (workspace_id, agent_id, box, status, created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_events_active_dedup ON agent_events (workspace_id, dedup_key) WHERE status NOT IN ('completed', 'dismissed')`,
		`
		CREATE TABLE IF NOT EXISTS agent_document_views (
			workspace_id UUID NOT NULL,
			agent_id UUID NOT NULL,
			document_id UUID NOT NULL,
			update_id BIGINT NOT NULL,
			state_vector TEXT NOT NULL,
			viewed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, agent_id, document_id),
			CONSTRAINT fk_agent_document_views_workspace
				FOREIGN KEY (workspace_id)
				REFERENCES workspaces(id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agent_document_views_agent
				FOREIGN KEY (workspace_id, agent_id)
				REFERENCES agents(workspace_id, id)
				ON DELETE CASCADE,
			CONSTRAINT fk_agent_document_views_document
				FOREIGN KEY (workspace_id, document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE CASCADE
		)
		`,
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("init postgres schema statement %d: %w", index+1, err)
		}
	}
	return nil
}

// initPostgresSchemaConstraints adds the cyclic FK and constraint triggers after
// the native UUID tables exist.
func initPostgresSchemaConstraints(db *sql.DB) error {
	statements := []string{
		// agents.current_run_id and agent_runs.agent_id form the only FK cycle, so
		// current_run is the single justified post-create FK. It is still validated on create.
		`DO $$
		BEGIN
			ALTER TABLE agents
				ADD CONSTRAINT fk_agents_current_run
				FOREIGN KEY (workspace_id, current_run_id)
				REFERENCES agent_runs(workspace_id, id)
				ON DELETE SET NULL (current_run_id)
				DEFERRABLE INITIALLY DEFERRED;
		EXCEPTION
			WHEN duplicate_object THEN NULL;
		END $$`,

		// ── Polymorphic constraint triggers ──

		// Generic workspace-scoped polymorphic ref guard.
		// TG_ARGV[0] = id column, TG_ARGV[1] = type column,
		// TG_ARGV[2..N] = pairs of (type_value, parent_table).
		`CREATE OR REPLACE FUNCTION check_workspace_ref()
		RETURNS TRIGGER AS $fn$
		DECLARE
			ref_id UUID;
			ref_type TEXT;
			found_row BOOLEAN;
		BEGIN
			EXECUTE format('SELECT ($1).%I, ($1).%I', TG_ARGV[0], TG_ARGV[1])
				INTO ref_id, ref_type USING NEW;
			IF ref_id IS NULL OR ref_type IS NULL OR ref_type = '' OR ref_type = 'system' THEN
				RETURN NEW;
			END IF;
			FOR i IN 2 .. TG_NARGS-1 BY 2 LOOP
				IF ref_type = TG_ARGV[i] THEN
					EXECUTE format('SELECT EXISTS(SELECT 1 FROM %I WHERE id = $1 AND workspace_id = $2)', TG_ARGV[i+1])
						INTO found_row USING ref_id, NEW.workspace_id;
					IF NOT found_row THEN
						RAISE EXCEPTION '% % references missing % in workspace %', TG_ARGV[0], ref_id, TG_ARGV[i], NEW.workspace_id;
					END IF;
					RETURN NEW;
				END IF;
			END LOOP;
			RAISE EXCEPTION '% has unrecognized type "%" for %', TG_ARGV[0], ref_type, TG_ARGV[1];
		END;
		$fn$ LANGUAGE plpgsql`,

		`DROP TRIGGER IF EXISTS trg_document_updates_actor_ref ON document_updates`,
		`CREATE TRIGGER trg_document_updates_actor_ref
			BEFORE INSERT OR UPDATE ON document_updates
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('actor_id', 'actor_type', 'human', 'users', 'agent', 'agents', 'daemon', 'daemons')`,

		`DROP TRIGGER IF EXISTS trg_presences_actor_ref ON presences`,
		`CREATE TRIGGER trg_presences_actor_ref
			BEFORE INSERT OR UPDATE ON presences
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('actor_id', 'actor_type', 'human', 'users', 'agent', 'agents', 'daemon', 'daemons')`,

		`DROP TRIGGER IF EXISTS trg_activities_actor_ref ON activities`,
		`CREATE TRIGGER trg_activities_actor_ref
			BEFORE INSERT OR UPDATE ON activities
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('actor_id', 'actor_type', 'human', 'users', 'agent', 'agents', 'daemon', 'daemons')`,

		`DROP TRIGGER IF EXISTS trg_threads_author_ref ON threads`,
		`CREATE TRIGGER trg_threads_author_ref
			BEFORE INSERT OR UPDATE ON threads
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('created_by_id', 'created_by_type', 'human', 'users', 'agent', 'agents')`,

		`DROP TRIGGER IF EXISTS trg_thread_messages_author_ref ON thread_messages`,
		`CREATE TRIGGER trg_thread_messages_author_ref
			BEFORE INSERT OR UPDATE ON thread_messages
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('author_id', 'author_type', 'human', 'users', 'agent', 'agents')`,

		`DROP TRIGGER IF EXISTS trg_activities_provenance_ref ON activities`,
		`CREATE TRIGGER trg_activities_provenance_ref
			BEFORE INSERT OR UPDATE ON activities
			FOR EACH ROW EXECUTE FUNCTION check_workspace_ref('provenance_actor_id', 'provenance_actor_type', 'human', 'users', 'agent', 'agents', 'daemon', 'daemons')`,

		// Union ref guard: participant_id must exist in users OR agents (no type column).
		`CREATE OR REPLACE FUNCTION check_participant_ref()
		RETURNS TRIGGER AS $fn$
		BEGIN
			PERFORM 1 FROM users WHERE id = NEW.participant_id AND workspace_id = NEW.workspace_id;
			IF FOUND THEN RETURN NEW; END IF;
			PERFORM 1 FROM agents WHERE id = NEW.participant_id AND workspace_id = NEW.workspace_id;
			IF FOUND THEN RETURN NEW; END IF;
			RAISE EXCEPTION 'participant_id % not found in users or agents in workspace %', NEW.participant_id, NEW.workspace_id;
		END;
		$fn$ LANGUAGE plpgsql`,

		`DROP TRIGGER IF EXISTS trg_thread_participants_ref ON thread_participants`,
		`CREATE TRIGGER trg_thread_participants_ref
			BEFORE INSERT OR UPDATE ON thread_participants
			FOR EACH ROW EXECUTE FUNCTION check_participant_ref()`,

		// Relationship guard: claimed_by must be the event's agent or that agent's daemon.
		`CREATE OR REPLACE FUNCTION check_agent_event_claimed_by()
		RETURNS TRIGGER AS $fn$
		BEGIN
			IF NEW.claimed_by IS NULL THEN
				RETURN NEW;
			END IF;
			IF NEW.claimed_by = NEW.agent_id THEN
				RETURN NEW;
			END IF;
			PERFORM 1 FROM agents WHERE id = NEW.agent_id AND daemon_id = NEW.claimed_by;
			IF FOUND THEN RETURN NEW; END IF;
			RAISE EXCEPTION 'claimed_by % is not the event agent % or its daemon', NEW.claimed_by, NEW.agent_id;
		END;
		$fn$ LANGUAGE plpgsql`,

		`DROP TRIGGER IF EXISTS trg_agent_events_claimed_by ON agent_events`,
		`CREATE TRIGGER trg_agent_events_claimed_by
			BEFORE INSERT OR UPDATE ON agent_events
			FOR EACH ROW EXECUTE FUNCTION check_agent_event_claimed_by()`,

		// ── Cleanup triggers for polymorphic ON DELETE behavior ──

		// Generic actor cleanup: nulls polymorphic refs and removes presences/participants.
		// TG_ARGV[0] = actor_type value used in type columns.
		`CREATE OR REPLACE FUNCTION on_actor_delete()
		RETURNS TRIGGER AS $fn$
		BEGIN
			-- When the whole workspace is being deleted, its row is already gone
			-- by the time the member-row cascade fires this trigger. Touching
			-- sibling rows then re-validates their workspace FK against a deleted
			-- workspace and aborts the cascade — so actor cleanup only applies
			-- while the workspace survives; otherwise the cascade removes
			-- everything anyway.
			IF NOT EXISTS (SELECT 1 FROM workspaces WHERE id = OLD.workspace_id) THEN
				RETURN OLD;
			END IF;
			UPDATE document_updates SET actor_id = NULL WHERE actor_type = TG_ARGV[0] AND actor_id = OLD.id;
			UPDATE threads SET created_by_id = NULL WHERE created_by_type = TG_ARGV[0] AND created_by_id = OLD.id;
			UPDATE thread_messages SET author_id = NULL WHERE author_type = TG_ARGV[0] AND author_id = OLD.id;
			UPDATE activities SET actor_id = NULL WHERE actor_type = TG_ARGV[0] AND actor_id = OLD.id;
			UPDATE activities SET provenance_actor_id = NULL WHERE provenance_actor_type = TG_ARGV[0] AND provenance_actor_id = OLD.id;
			DELETE FROM presences WHERE actor_type = TG_ARGV[0] AND actor_id = OLD.id;
			DELETE FROM thread_participants WHERE participant_id = OLD.id;
			RETURN OLD;
		END;
		$fn$ LANGUAGE plpgsql`,

		`DROP TRIGGER IF EXISTS trg_user_delete_cleanup ON users`,
		`CREATE TRIGGER trg_user_delete_cleanup
			BEFORE DELETE ON users
			FOR EACH ROW EXECUTE FUNCTION on_actor_delete('human')`,

		`DROP TRIGGER IF EXISTS trg_agent_delete_cleanup ON agents`,
		`CREATE TRIGGER trg_agent_delete_cleanup
			BEFORE DELETE ON agents
			FOR EACH ROW EXECUTE FUNCTION on_actor_delete('agent')`,

		// Daemon cleanup: subset policy (no threads/thread_messages/participants).
		`CREATE OR REPLACE FUNCTION on_daemon_delete()
		RETURNS TRIGGER AS $fn$
		BEGIN
			-- Same workspace-cascade guard as on_actor_delete above.
			IF NOT EXISTS (SELECT 1 FROM workspaces WHERE id = OLD.workspace_id) THEN
				RETURN OLD;
			END IF;
			UPDATE document_updates SET actor_id = NULL WHERE actor_type = 'daemon' AND actor_id = OLD.id;
			UPDATE activities SET actor_id = NULL WHERE actor_type = 'daemon' AND actor_id = OLD.id;
			UPDATE activities SET provenance_actor_id = NULL WHERE provenance_actor_type = 'daemon' AND provenance_actor_id = OLD.id;
			DELETE FROM presences WHERE actor_type = 'daemon' AND actor_id = OLD.id;
			RETURN OLD;
		END;
		$fn$ LANGUAGE plpgsql`,

		`DROP TRIGGER IF EXISTS trg_daemon_delete_cleanup ON daemons`,
		`CREATE TRIGGER trg_daemon_delete_cleanup
			BEFORE DELETE ON daemons
			FOR EACH ROW EXECUTE FUNCTION on_daemon_delete()`,
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("init postgres schema constraint %d: %w", index+1, err)
		}
	}
	// documents.workspace_id and workspaces.root_document_id form the schema's
	// second FK cycle. Unlike agents↔agent_runs it cannot break via a nullable
	// side (root_document_id is NOT NULL), so it defers instead: a workspace and
	// its root commit together and the constraint validates at commit. Deleting a
	// live workspace's root document is refused (RESTRICT).
	if _, err := db.Exec(
		`DO $$
		BEGIN
			ALTER TABLE workspaces
				ADD CONSTRAINT fk_workspaces_root_document
				FOREIGN KEY (id, root_document_id)
				REFERENCES documents(workspace_id, id)
				ON DELETE RESTRICT
				DEFERRABLE INITIALLY DEFERRED;
		EXCEPTION
			WHEN duplicate_object THEN NULL;
		END $$`,
	); err != nil {
		return fmt.Errorf("add fk_workspaces_root_document: %w", err)
	}
	return nil
}

func (s *Store) loadNormalizedPostgresLocked() error {
	s.state.ContentDocuments = map[string]*Document{}
	s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}

	if err := s.ensurePostgresCheckpointsLocked(); err != nil {
		return fmt.Errorf("ensure checkpoints: %w", err)
	}
	if err := s.loadDocumentsPostgresLocked(); err != nil {
		return fmt.Errorf("load documents: %w", err)
	}
	return nil
}

func (s *Store) loadWorkspaceMetadataPostgresLocked() error {
	var name string
	var rootDocumentID string
	err := s.db.QueryRow(
		`SELECT name, root_document_id::text FROM workspaces WHERE id = $1::uuid`,
		s.state.WorkspaceID,
	).Scan(&name, &rootDocumentID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) != "" {
		s.state.Name = name
	}
	s.state.RootDocumentID = rootDocumentID
	return nil
}

func (s *Store) persistDocumentMutationPostgresLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	inboxEvents, err := s.persistDocumentsPostgresLocked(tx)
	if err != nil {
		return err
	}
	// Flush activities in the same transaction as the documents they reference
	// (e.g. document.created), so the activities FK is satisfied atomically.
	if err = s.insertPendingActivitiesPostgresLocked(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.recordActivitiesCreatedLocked(s.pendingActivities)
	s.dirtyDocuments = map[string]struct{}{}
	s.pendingDocumentEvents = []documentUpdateRecord{}
	s.pendingActivities = nil
	for _, event := range inboxEvents {
		s.recordAgentInboxChangedLocked(event)
	}
	return nil
}

func (s *Store) pendingDocumentMutationIDsLocked() []string {
	ids := make([]string, 0, len(s.dirtyDocuments))
	for documentID := range s.dirtyDocuments {
		if documentID == "" {
			continue
		}
		ids = append(ids, documentID)
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) persistDocumentsPostgresLocked(tx *sql.Tx) ([]*AgentEvent, error) {
	var inboxEvents []*AgentEvent
	for documentID := range s.dirtyDocuments {
		document := s.state.ContentDocuments[documentID]
		if document == nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO documents (workspace_id, id, hidden, client_id_seed, create_client_operation_id, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id)
			 DO UPDATE SET hidden = EXCLUDED.hidden,
			               client_id_seed = EXCLUDED.client_id_seed,
			               create_client_operation_id = EXCLUDED.create_client_operation_id,
			               updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
			document.ID,
			document.Hidden,
			int64(document.ClientIDSeed),
			document.CreateClientOperationID,
			document.UpdatedAt,
		); err != nil {
			return nil, err
		}
	}
	latestUpdateByDocument := map[string]int64{}
	latestMetaByDocument := map[string]OperationMeta{}
	for _, event := range s.pendingDocumentEvents {
		var updateID int64
		if err := tx.QueryRow(
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id`,
			s.state.WorkspaceID,
			event.DocumentID,
			event.Update,
			actorUUIDOrNil(event.ActorID, event.ActorType),
			event.ActorType,
			event.CreatedAt,
		).Scan(&updateID); err != nil {
			return nil, err
		}
		latestUpdateByDocument[event.DocumentID] = updateID
		latestMetaByDocument[event.DocumentID] = OperationMeta{
			ActorID:   event.ActorID,
			ActorType: event.ActorType,
			Source:    "document-update",
		}
	}
	for documentID, updateID := range latestUpdateByDocument {
		if document := s.state.ContentDocuments[documentID]; document != nil {
			document.UpdateID = updateID
			events, err := s.enqueueDocumentInboxEventsLocked(tx, document, latestMetaByDocument[documentID])
			if err != nil {
				return nil, err
			}
			inboxEvents = append(inboxEvents, events...)
		}
	}
	for documentID := range s.dirtyDocuments {
		document := s.state.ContentDocuments[documentID]
		if document == nil {
			continue
		}
		_, hasIncrementalUpdate := latestUpdateByDocument[documentID]
		if hasIncrementalUpdate {
			result, err := tx.Exec(
				`UPDATE document_heads
				    SET state_vector = $1,
				        update_id = $2,
				        updated_at = $3
				  WHERE workspace_id = $4::uuid AND document_id = $5::uuid`,
				document.StateVector,
				document.UpdateID,
				document.UpdatedAt,
				s.state.WorkspaceID,
				document.ID,
			)
			if err != nil {
				return nil, err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return nil, err
			}
			if affected == 0 {
				if _, err := tx.Exec(
					`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
					 VALUES ($1, $2, $3, $4, $5)`,
					s.state.WorkspaceID,
					document.ID,
					document.StateVector,
					document.UpdateID,
					document.UpdatedAt,
				); err != nil {
					return nil, err
				}
			}
		} else if _, err := tx.Exec(
			`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (document_id)
			 DO UPDATE SET state_vector = EXCLUDED.state_vector,
			               update_id = EXCLUDED.update_id,
			               updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
			document.ID,
			document.StateVector,
			document.UpdateID,
			document.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := s.maybeInsertDocumentCheckpointPostgresLocked(tx, document); err != nil {
			return nil, err
		}
	}
	return inboxEvents, nil
}

func (s *Store) maybeInsertDocumentCheckpointPostgresLocked(tx *sql.Tx, document *Document) error {
	if document == nil || document.UpdateID <= 0 {
		return nil
	}
	var lastCheckpointID int64
	err := tx.QueryRow(
		`SELECT COALESCE(MAX(update_id), 0)
		   FROM document_checkpoints
		  WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		s.state.WorkspaceID,
		document.ID,
	).Scan(&lastCheckpointID)
	if err != nil {
		return err
	}
	if lastCheckpointID != 0 && document.UpdateID-lastCheckpointID < postgresCheckpointInterval {
		return nil
	}
	stateVector, err := s.insertPostgresCheckpointAtHeadTxLocked(tx, document.ID, document.ClientIDSeed, document.UpdateID)
	if err != nil {
		return err
	}
	document.StateVector = stateVector
	return nil
}

func insertAgentPostgres(tx *sql.Tx, workspaceID string, agent *Agent) error {
	if agent == nil {
		return nil
	}
	_, err := tx.Exec(
		`INSERT INTO agents (
			workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
			current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16, $17, $18
		)`,
		workspaceID,
		agent.ID,
		uuidStringOrNil(agent.DaemonID),
		agent.Handle,
		agent.Name,
		agent.Role,
		agent.Kind,
		agent.SystemPrompt,
		agent.WorkspaceRoot,
		agent.CurrentTurnID,
		agent.SessionID,
		agent.Status,
		agent.CurrentTask,
		agent.CurrentActivity,
		uuidStringOrNil(agent.CurrentRunID),
		nullTime(agent.LastHeartbeatAt),
		nullTime(agent.LastRunCompleted),
		agent.UpdatedAt,
	)
	return err
}

func upsertAgentPostgresTx(tx *sql.Tx, workspaceID string, agent *Agent) error {
	if agent == nil {
		return nil
	}
	_, err := tx.Exec(
		`INSERT INTO agents (
			workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
			current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14,
			$15, $16, $17, $18
		)
		ON CONFLICT (id)
		DO UPDATE SET daemon_id = EXCLUDED.daemon_id,
		              handle = EXCLUDED.handle,
		              name = EXCLUDED.name,
		              role = EXCLUDED.role,
		              kind = EXCLUDED.kind,
		              system_prompt = EXCLUDED.system_prompt,
		              workspace_root = EXCLUDED.workspace_root,
		              current_turn_id = EXCLUDED.current_turn_id,
		              session_id = EXCLUDED.session_id,
		              status = EXCLUDED.status,
		              current_task = EXCLUDED.current_task,
		              current_activity = EXCLUDED.current_activity,
		              current_run_id = EXCLUDED.current_run_id,
		              last_heartbeat_at = EXCLUDED.last_heartbeat_at,
		              last_run_completed = EXCLUDED.last_run_completed,
		              updated_at = EXCLUDED.updated_at`,
		workspaceID,
		agent.ID,
		uuidStringOrNil(agent.DaemonID),
		agent.Handle,
		agent.Name,
		agent.Role,
		agent.Kind,
		agent.SystemPrompt,
		agent.WorkspaceRoot,
		agent.CurrentTurnID,
		agent.SessionID,
		agent.Status,
		agent.CurrentTask,
		agent.CurrentActivity,
		uuidStringOrNil(agent.CurrentRunID),
		nullTime(agent.LastHeartbeatAt),
		nullTime(agent.LastRunCompleted),
		agent.UpdatedAt,
	)
	return err
}

func upsertPresencePostgres(db *sql.DB, workspaceID string, presence *Presence) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	start, end := selectionBounds(presence.Selection)
	if _, err = tx.Exec(
		`INSERT INTO presences (
			workspace_id, actor_id, actor_type, document_id, file_path, mode,
			selection_start, selection_end, activity, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10
		)
		ON CONFLICT (workspace_id, actor_id)
		DO UPDATE SET
			actor_type = EXCLUDED.actor_type,
			document_id = EXCLUDED.document_id,
			file_path = EXCLUDED.file_path,
			mode = EXCLUDED.mode,
			selection_start = EXCLUDED.selection_start,
			selection_end = EXCLUDED.selection_end,
			activity = EXCLUDED.activity,
			updated_at = EXCLUDED.updated_at`,
		workspaceID,
		presence.ActorID,
		presence.ActorType,
		uuidStringOrNil(presence.DocumentID),
		presence.FilePath,
		presence.Mode,
		start,
		end,
		presence.Activity,
		presence.UpdatedAt,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func listPresencesPostgres(q querier, workspaceID string) ([]*Presence, error) {
	rows, err := q.Query(
		`SELECT actor_id::text, actor_type, COALESCE(document_id::text, ''), file_path, mode, selection_start, selection_end, activity, updated_at
		   FROM presences
		  WHERE workspace_id = $1::uuid
		    AND updated_at > now() - interval '2 minutes'`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	presences := make([]*Presence, 0)
	for rows.Next() {
		p := &Presence{}
		var start sql.NullInt64
		var end sql.NullInt64
		if err := rows.Scan(&p.ActorID, &p.ActorType, &p.DocumentID, &p.FilePath, &p.Mode, &start, &end, &p.Activity, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Selection = selectionFromNulls(start, end)
		presences = append(presences, p)
	}
	return presences, rows.Err()
}

// activityExecer is satisfied by both *sql.DB and *sql.Tx, so an activity can
// be inserted directly or inside an operation's persist transaction.
type activityExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertActivityPostgres(exec activityExecer, workspaceID string, activity *ActivityEvent) error {
	if activity == nil {
		return nil
	}
	_, err := exec.Exec(
		`INSERT INTO activities (
			workspace_id, type, document_id, actor_id, actor_type, summary, occurred_at,
			provenance_actor_id, provenance_actor_type, provenance_execution_id, provenance_tool,
				provenance_trigger, provenance_autonomous, provenance_confidence, provenance_requested_by,
				provenance_source, provenance_intended_scope, provenance_read_set_summary,
				presence_ref
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11,
				$12, $13, $14, $15,
				$16, $17, $18,
				$19
			)`,
		workspaceID,
		activity.Type,
		uuidStringOrNil(activity.DocumentID),
		actorUUIDOrNil(activity.ActorID, activity.ActorType),
		activity.ActorType,
		activity.Summary,
		activity.OccurredAt,
		actorUUIDOrNil(activity.Provenance.ActorID, activity.Provenance.ActorType),
		activity.Provenance.ActorType,
		activity.Provenance.ExecutionID,
		activity.Provenance.Tool,
		activity.Provenance.Trigger,
		activity.Provenance.Autonomous,
		activity.Provenance.Confidence,
		activity.Provenance.RequestedBy,
		activity.Provenance.Source,
		activity.Provenance.IntendedScope,
		activity.Provenance.ReadSetSummary,
		activity.PresenceRef,
	)
	return err
}

func (s *Store) insertPendingActivitiesPostgresLocked(tx *sql.Tx) error {
	for _, activity := range s.pendingActivities {
		if err := insertActivityPostgres(tx, s.state.WorkspaceID, activity); err != nil {
			return err
		}
	}
	return nil
}

func claimAgentEventPostgres(db *sql.DB, workspaceID string, agentID string, agentHandle string, claimedBy string) (*AgentEvent, error) {
	now := time.Now().UTC()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	row := tx.QueryRow(
		`WITH next AS (
				SELECT id
			  FROM agent_events
			 WHERE workspace_id = $1
			   AND agent_id = $2
			   AND available_at <= $3
			   AND status NOT IN ('completed', 'dismissed')
			   AND NOT (status = 'processing' AND claimed_at > $4)
			 ORDER BY created_at ASC
			 LIMIT 1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE agent_events
		   SET status = 'processing',
		       agent_handle = $5,
		       claimed_by = $6,
		       claimed_at = $3,
		       attempt_count = agent_events.attempt_count + 1,
		       updated_at = $3
		  FROM next
		 WHERE agent_events.id = next.id
			RETURNING agent_events.id::text, agent_events.agent_id::text, agent_events.agent_handle, agent_events.type,
			          agent_events.box, agent_events.status, COALESCE(agent_events.document_id::text, ''), COALESCE(agent_events.thread_id::text, ''),
			          COALESCE(agent_events.thread_message_id::text, ''), agent_events.from_update_id, agent_events.to_update_id, agent_events.summary,
			          agent_events.prompt, agent_events.dedup_key,
			          COALESCE(agent_events.claimed_by::text, ''), COALESCE(agent_events.run_id::text, ''), agent_events.last_error,
			          agent_events.attempt_count, agent_events.available_at, agent_events.claimed_at,
			          agent_events.completed_at, agent_events.created_at, agent_events.updated_at`,
		workspaceID,
		agentID,
		now,
		now.Add(-30*time.Second),
		agentHandle,
		uuidStringOrNil(claimedBy),
	)
	event, scanErr := scanAgentEvent(row)
	if scanErr != nil {
		if scanErr == sql.ErrNoRows {
			_ = tx.Rollback()
			return nil, ErrNotFound
		}
		err = scanErr
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return cloneAgentEvent(event), nil
}

func updateAgentEventPostgres(db *sql.DB, workspaceID string, id string, req UpdateAgentEventRequest) (*AgentEvent, error) {
	now := time.Now().UTC()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	row := tx.QueryRow(
		`UPDATE agent_events
		    SET status = CASE WHEN $1 <> '' THEN $1 ELSE status END,
		        thread_id = COALESCE(NULLIF($2, '')::uuid, thread_id),
		        run_id = COALESCE(NULLIF($3, '')::uuid, run_id),
		        last_error = CASE WHEN $4 <> '' THEN $4 ELSE last_error END,
		        completed_at = CASE WHEN $1 = 'completed' THEN $5 ELSE completed_at END,
		        available_at = CASE WHEN $1 = 'pending' AND available_at < $5 THEN $6 ELSE available_at END,
		        updated_at = $5
		  WHERE workspace_id = $7
		    AND id = $8
		RETURNING id::text, agent_id::text, agent_handle, type, box, status, COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
		          COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
		          summary, prompt, dedup_key, COALESCE(claimed_by::text, ''), COALESCE(run_id::text, ''), last_error, attempt_count,
		          available_at, claimed_at, completed_at, created_at, updated_at`,
		strings.TrimSpace(req.Status),
		strings.TrimSpace(req.ThreadID),
		strings.TrimSpace(req.RunID),
		strings.TrimSpace(req.LastError),
		now,
		now.Add(5*time.Second),
		workspaceID,
		id,
	)
	updated, scanErr := scanAgentEvent(row)
	if scanErr != nil {
		if scanErr == sql.ErrNoRows {
			_ = tx.Rollback()
			return nil, ErrNotFound
		}
		err = scanErr
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return cloneAgentEvent(updated), nil
}

func upsertAgentDocumentViewPostgres(exec activityExecer, workspaceID string, view *AgentDocumentView) error {
	if view == nil {
		return nil
	}
	_, err := exec.Exec(
		`INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (workspace_id, agent_id, document_id)
		 DO UPDATE SET update_id = EXCLUDED.update_id,
		               state_vector = EXCLUDED.state_vector,
		               viewed_at = EXCLUDED.viewed_at`,
		workspaceID,
		view.AgentID,
		view.DocumentID,
		view.UpdateID,
		view.StateVector,
		view.ViewedAt,
	)
	return err
}

func completeDocumentInboxEventsPostgres(exec activityExecer, workspaceID string, agentID string, documentID string, updateID int64, completedAt time.Time) error {
	_, err := exec.Exec(
		`UPDATE agent_events
		    SET status = 'completed',
		        completed_at = $5,
		        updated_at = $5
		  WHERE workspace_id = $1
		    AND agent_id = $2
		    AND document_id = $3
		    AND type LIKE 'document.%'
		    AND status NOT IN ('completed', 'dismissed')
		    AND to_update_id <= $4`,
		workspaceID,
		agentID,
		documentID,
		updateID,
		completedAt,
	)
	return err
}

func getAgentDocumentViewPostgres(q querier, workspaceID string, agentID string, documentID string) (*AgentDocumentView, error) {
	if !isUUIDString(agentID) || !isUUIDString(documentID) {
		return nil, ErrNotFound
	}
	row := q.QueryRow(
		`SELECT agent_id::text, document_id::text, update_id, state_vector, viewed_at
		   FROM agent_document_views
		  WHERE workspace_id = $1::uuid
		    AND agent_id = $2::uuid
		    AND document_id = $3::uuid`,
		workspaceID,
		agentID,
		documentID,
	)
	view := &AgentDocumentView{}
	if err := row.Scan(&view.AgentID, &view.DocumentID, &view.UpdateID, &view.StateVector, &view.ViewedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return view, nil
}

func getAgentEventPostgres(db *sql.DB, workspaceID, id string) (*AgentEvent, error) {
	row := db.QueryRow(
		`SELECT id::text, agent_id::text, agent_handle, type, box, status,
		        COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
		        COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
		        summary, prompt, dedup_key, COALESCE(claimed_by::text, ''),
		        COALESCE(run_id::text, ''), last_error, attempt_count,
		        available_at, claimed_at, completed_at, created_at, updated_at
		   FROM agent_events
		  WHERE workspace_id = $1::uuid AND id = $2::uuid`,
		workspaceID, id,
	)
	event, err := scanAgentEvent(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return event, err
}

func listAgentEventsPostgres(db *sql.DB, workspaceID, agentID string, box string, statuses []string) ([]*AgentEvent, error) {
	query := `SELECT id::text, agent_id::text, agent_handle, type, box, status,
	                 COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
	                 COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
	                 summary, prompt, dedup_key, COALESCE(claimed_by::text, ''),
	                 COALESCE(run_id::text, ''), last_error, attempt_count,
	                 available_at, claimed_at, completed_at, created_at, updated_at
	            FROM agent_events
	           WHERE workspace_id = $1::uuid AND agent_id = $2::uuid`
	args := []any{workspaceID, agentID}
	argN := 3
	if box != "" {
		query += fmt.Sprintf(` AND box = $%d`, argN)
		args = append(args, box)
		argN++
	}
	if len(statuses) > 0 {
		query += fmt.Sprintf(` AND status IN (`)
		for i, s := range statuses {
			if i > 0 {
				query += ","
			}
			query += fmt.Sprintf("$%d", argN)
			args = append(args, s)
			argN++
		}
		query += ")"
	}
	query += ` ORDER BY created_at ASC, id ASC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]*AgentEvent, 0)
	for rows.Next() {
		event, err := scanAgentEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func listAllAgentEventsPostgres(q querier, workspaceID string) ([]*AgentEvent, error) {
	rows, err := q.Query(
		`SELECT id::text, agent_id::text, agent_handle, type, box, status,
		        COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
		        COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
		        summary, prompt, dedup_key, COALESCE(claimed_by::text, ''),
		        COALESCE(run_id::text, ''), last_error, attempt_count,
		        available_at, claimed_at, completed_at, created_at, updated_at
		   FROM agent_events
		  WHERE workspace_id = $1::uuid
		  ORDER BY created_at ASC, id ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]*AgentEvent, 0)
	for rows.Next() {
		event, err := scanAgentEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func insertAgentEventTx(tx *sql.Tx, workspaceID string, event *AgentEvent) error {
	_, err := tx.Exec(
		`INSERT INTO agent_events (
			workspace_id, id, agent_id, agent_handle, type, box, status, document_id,
			thread_id, thread_message_id, from_update_id, to_update_id, summary,
			prompt, dedup_key, claimed_by, run_id, last_error, attempt_count,
			available_at, claimed_at, completed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23, $24
		)
		ON CONFLICT (workspace_id, dedup_key) WHERE status NOT IN ('completed', 'dismissed')
		DO NOTHING`,
		workspaceID,
		event.ID,
		event.AgentID,
		event.AgentHandle,
		event.Type,
		normalizeInboxBox(event.Box),
		event.Status,
		uuidStringOrNil(event.DocumentID),
		uuidStringOrNil(event.ThreadID),
		uuidStringOrNil(event.ThreadMessageID),
		event.FromUpdateID,
		event.ToUpdateID,
		event.Summary,
		event.Prompt,
		event.DedupKey,
		uuidStringOrNil(event.ClaimedBy),
		uuidStringOrNil(event.RunID),
		event.LastError,
		event.AttemptCount,
		event.AvailableAt,
		nullTime(event.ClaimedAt),
		nullTime(event.CompletedAt),
		event.CreatedAt,
		event.UpdatedAt,
	)
	return err
}

func upsertDocumentInboxEventPostgres(q querier, workspaceID string, event *AgentEvent) (*AgentEvent, error) {
	row := q.QueryRow(
		`INSERT INTO agent_events (
			workspace_id, id, agent_id, agent_handle, type, box, status, document_id,
			thread_id, thread_message_id, from_update_id, to_update_id, summary,
			prompt, dedup_key, claimed_by, run_id, last_error, attempt_count,
			available_at, claimed_at, completed_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23, $24
		)
		ON CONFLICT (workspace_id, dedup_key) WHERE status NOT IN ('completed', 'dismissed')
		DO UPDATE SET
			agent_handle = EXCLUDED.agent_handle,
			thread_id = EXCLUDED.thread_id,
			from_update_id = LEAST(EXCLUDED.from_update_id, agent_events.from_update_id),
			to_update_id = GREATEST(EXCLUDED.to_update_id, agent_events.to_update_id),
			summary = EXCLUDED.summary,
			prompt = EXCLUDED.prompt,
			available_at = EXCLUDED.available_at,
			updated_at = EXCLUDED.updated_at
		RETURNING id::text, agent_id::text, agent_handle, type, box, status,
		          COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
		          COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
		          summary, prompt, dedup_key, COALESCE(claimed_by::text, ''),
		          COALESCE(run_id::text, ''), last_error, attempt_count,
		          available_at, claimed_at, completed_at, created_at, updated_at`,
		workspaceID,
		event.ID,
		event.AgentID,
		event.AgentHandle,
		event.Type,
		normalizeInboxBox(event.Box),
		event.Status,
		uuidStringOrNil(event.DocumentID),
		uuidStringOrNil(event.ThreadID),
		uuidStringOrNil(event.ThreadMessageID),
		event.FromUpdateID,
		event.ToUpdateID,
		event.Summary,
		event.Prompt,
		event.DedupKey,
		uuidStringOrNil(event.ClaimedBy),
		uuidStringOrNil(event.RunID),
		event.LastError,
		event.AttemptCount,
		event.AvailableAt,
		nullTime(event.ClaimedAt),
		nullTime(event.CompletedAt),
		event.CreatedAt,
		event.UpdatedAt,
	)
	return scanAgentEvent(row)
}

func documentInboxHandledPostgres(db *sql.DB, workspaceID, agentID, documentID string, updateID int64, updatedAt time.Time) (bool, error) {
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM agent_events
			 WHERE workspace_id = $1::uuid
			   AND agent_id = $2::uuid
			   AND document_id = $3::uuid
			   AND type LIKE 'document.%'
			   AND status IN ('completed', 'dismissed')
			   AND (to_update_id >= $4 OR (to_update_id = 0 AND updated_at >= $5))
		)`,
		workspaceID, agentID, documentID, updateID, updatedAt,
	).Scan(&exists)
	return exists, err
}

func documentContentAtUpdatePostgres(db *sql.DB, workspaceID string, document *Document, updateID int64) (string, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(document.ClientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	err := db.QueryRow(
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id = $1::uuid AND document_id = $2::uuid AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		workspaceID,
		document.ID,
		updateID,
	).Scan(&appliedThrough, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil && checkpointState != "" {
		checkpointUpdate, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			return "", decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, checkpointUpdate, "checkpoint"); applyErr != nil {
			return "", applyErr
		}
	}
	rows, err := db.Query(
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id = $1::uuid
		    AND document_id = $2::uuid
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		workspaceID,
		document.ID,
		appliedThrough,
		updateID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return "", err
		}
		if err := crdt.ApplyUpdateV1(doc, update, "history"); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return doc.GetText("content").ToString(), nil
}

func (s *Store) ensurePostgresCheckpointsLocked() error {
	rows, err := s.db.Query(
		`SELECT d.id::text,
		        d.client_id_seed,
		        h.update_id,
		        COALESCE(checkpoint.update_id, 0) AS checkpoint_update_id
		   FROM documents d
		   JOIN document_heads h
		     ON h.workspace_id = d.workspace_id AND h.document_id = d.id
		   LEFT JOIN LATERAL (
		       SELECT update_id
		         FROM document_checkpoints c
		        WHERE c.workspace_id = d.workspace_id
		          AND c.document_id = d.id
		          AND c.update_id <= h.update_id
		        ORDER BY c.update_id DESC
		        LIMIT 1
		   ) checkpoint ON TRUE
		  WHERE d.workspace_id = $1::uuid
		    AND h.update_id > 0
		    AND (checkpoint.update_id IS NULL OR h.update_id - checkpoint.update_id > $2)
		  ORDER BY d.id ASC`,
		s.state.WorkspaceID,
		postgresCheckpointTailLimit,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	type checkpointTarget struct {
		documentID         string
		clientIDSeed       uint64
		headUpdateID       int64
		checkpointUpdateID int64
	}
	targets := []checkpointTarget{}
	for rows.Next() {
		var target checkpointTarget
		var clientIDSeed int64
		if err := rows.Scan(&target.documentID, &clientIDSeed, &target.headUpdateID, &target.checkpointUpdateID); err != nil {
			return err
		}
		target.clientIDSeed = uint64(clientIDSeed)
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, target := range targets {
		if err := s.insertPostgresCheckpointAtHeadLocked(target.documentID, target.clientIDSeed, target.headUpdateID); err != nil {
			return fmt.Errorf("checkpoint %s at update %d: %w", target.documentID, target.headUpdateID, err)
		}
	}
	return nil
}

func (s *Store) insertPostgresCheckpointAtHeadLocked(documentID string, clientIDSeed uint64, headUpdateID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.insertPostgresCheckpointAtHeadTxLocked(tx, documentID, clientIDSeed, headUpdateID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) insertPostgresCheckpointAtHeadTxLocked(tx *sql.Tx, documentID string, clientIDSeed uint64, headUpdateID int64) (string, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(clientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	var checkpointUpdateID int64
	err := tx.QueryRow(
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id = $1::uuid AND document_id = $2::uuid AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		s.state.WorkspaceID,
		documentID,
		headUpdateID,
	).Scan(&checkpointUpdateID, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil && checkpointState != "" {
		update, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			return "", decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, update, "checkpoint"); applyErr != nil {
			return "", applyErr
		}
		appliedThrough = checkpointUpdateID
	}

	rows, err := tx.Query(
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id = $1::uuid
		    AND document_id = $2::uuid
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		s.state.WorkspaceID,
		documentID,
		appliedThrough,
		headUpdateID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return "", err
		}
		if err := crdt.ApplyUpdateV1(doc, update, "checkpoint-tail"); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	crdtState := base64.StdEncoding.EncodeToString(doc.EncodeStateAsUpdate())
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	if _, err := tx.Exec(
		`INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (workspace_id, document_id, update_id)
		 DO UPDATE SET crdt_state = EXCLUDED.crdt_state,
		               state_vector = EXCLUDED.state_vector,
		               created_at = EXCLUDED.created_at`,
		s.state.WorkspaceID,
		documentID,
		headUpdateID,
		crdtState,
		stateVector,
		time.Now().UTC(),
	); err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`UPDATE document_heads
		    SET state_vector = $1
		  WHERE workspace_id = $2 AND document_id = $3 AND update_id = $4`,
		stateVector,
		s.state.WorkspaceID,
		documentID,
		headUpdateID,
	); err != nil {
		return "", err
	}
	return stateVector, nil
}

func listUsersPostgres(q querier, workspaceID string) ([]*User, error) {
	rows, err := q.Query(
		`SELECT id::text, handle, name, role, kind, status, created_at, updated_at
		   FROM users
		  WHERE workspace_id = $1::uuid
		  ORDER BY handle ASC, id ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []*User{}
	for rows.Next() {
		user := &User{}
		if err := rows.Scan(
			&user.ID,
			&user.Handle,
			&user.Name,
			&user.Role,
			&user.Kind,
			&user.Status,
			&user.CreatedAt,
			&user.UpdatedAt,
		); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func listAgentsPostgres(q querier, workspaceID string) ([]*Agent, error) {
	rows, err := q.Query(
		`SELECT id::text, COALESCE(daemon_id::text, ''), handle, name, role, kind, system_prompt, workspace_root,
		        current_turn_id, session_id, status,
		        current_task, current_activity, COALESCE(current_run_id::text, ''), last_heartbeat_at,
		        last_run_completed, updated_at
		   FROM agents
		  WHERE workspace_id = $1::uuid
		  ORDER BY name ASC, id ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []*Agent{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func getAgentPostgres(q querier, workspaceID string, agentID string) (*Agent, error) {
	if !isUUIDString(agentID) {
		return nil, ErrNotFound
	}
	row := q.QueryRow(
		`SELECT id::text, COALESCE(daemon_id::text, ''), handle, name, role, kind, system_prompt, workspace_root,
		        current_turn_id, session_id, status,
		        current_task, current_activity, COALESCE(current_run_id::text, ''), last_heartbeat_at,
		        last_run_completed, updated_at
		   FROM agents
		  WHERE workspace_id = $1::uuid
		    AND id = $2::uuid`,
		workspaceID,
		agentID,
	)
	agent, err := scanAgent(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return agent, err
}

func getAgentForUpdatePostgres(tx *sql.Tx, workspaceID string, agentID string) (*Agent, error) {
	if !isUUIDString(agentID) {
		return nil, ErrNotFound
	}
	row := tx.QueryRow(
		`SELECT id::text, COALESCE(daemon_id::text, ''), handle, name, role, kind, system_prompt, workspace_root,
		        current_turn_id, session_id, status,
		        current_task, current_activity, COALESCE(current_run_id::text, ''), last_heartbeat_at,
		        last_run_completed, updated_at
		   FROM agents
		  WHERE workspace_id = $1::uuid
		    AND id = $2::uuid
		  FOR UPDATE`,
		workspaceID,
		agentID,
	)
	agent, err := scanAgent(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return agent, err
}

func resolveAgentIdentityPostgres(q querier, workspaceID string, agentRef string) (string, string, error) {
	trimmed := strings.TrimSpace(agentRef)
	row := q.QueryRow(
		`SELECT id::text, handle
		   FROM agents
		  WHERE workspace_id = $1::uuid
		    AND (handle = $2 OR ($3::uuid IS NOT NULL AND id = $3::uuid))`,
		workspaceID,
		trimmed,
		uuidStringOrNil(trimmed),
	)
	var id string
	var handle string
	if err := row.Scan(&id, &handle); err != nil {
		if err == sql.ErrNoRows {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	return id, handle, nil
}

func resolvePrincipalPostgres(q querier, workspaceID string, actorID string, actorType string) (*principalIdentity, error) {
	trimmedType := strings.TrimSpace(actorType)
	if trimmedType == "human" {
		principal, err := resolveUserPrincipalPostgres(q, workspaceID, actorID)
		if err != ErrNotFound {
			return principal, err
		}
	}
	if trimmedType == "agent" {
		principal, err := resolveAgentPrincipalPostgres(q, workspaceID, actorID)
		if err != ErrNotFound {
			return principal, err
		}
	}
	if principal, err := resolveUserPrincipalPostgres(q, workspaceID, actorID); err != ErrNotFound {
		return principal, err
	}
	if principal, err := resolveAgentPrincipalPostgres(q, workspaceID, actorID); err != ErrNotFound {
		return principal, err
	}
	return nil, ErrNotFound
}

func resolveUserPrincipalPostgres(q querier, workspaceID string, userRef string) (*principalIdentity, error) {
	trimmed := strings.TrimSpace(userRef)
	row := q.QueryRow(
		`SELECT id::text, handle, name
		   FROM users
		  WHERE workspace_id = $1::uuid
		    AND (handle = $2 OR ($3::uuid IS NOT NULL AND id = $3::uuid))`,
		workspaceID,
		trimmed,
		uuidStringOrNil(trimmed),
	)
	principal := &principalIdentity{Type: "human"}
	if err := row.Scan(&principal.ID, &principal.Handle, &principal.Name); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return principal, nil
}

func resolveAgentPrincipalPostgres(q querier, workspaceID string, agentRef string) (*principalIdentity, error) {
	trimmed := strings.TrimSpace(agentRef)
	row := q.QueryRow(
		`SELECT id::text, handle, name
		   FROM agents
		  WHERE workspace_id = $1::uuid
		    AND (handle = $2 OR ($3::uuid IS NOT NULL AND id = $3::uuid))`,
		workspaceID,
		trimmed,
		uuidStringOrNil(trimmed),
	)
	principal := &principalIdentity{Type: "agent"}
	if err := row.Scan(&principal.ID, &principal.Handle, &principal.Name); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return principal, nil
}

func principalByHandlePostgres(q querier, workspaceID string, handle string) (*principalRef, bool, error) {
	trimmed := strings.TrimSpace(handle)
	if trimmed == "" {
		return nil, false, nil
	}
	row := q.QueryRow(
		`SELECT id::text, handle, name, kind
		   FROM users
		  WHERE workspace_id = $1::uuid AND handle = $2`,
		workspaceID,
		trimmed,
	)
	user := &principalRef{}
	if err := row.Scan(&user.UserID, &user.Handle, &user.Name, &user.Kind); err == nil {
		return user, true, nil
	} else if err != sql.ErrNoRows {
		return nil, false, err
	}
	row = q.QueryRow(
		`SELECT id::text, handle, name
		   FROM agents
		  WHERE workspace_id = $1::uuid AND handle = $2`,
		workspaceID,
		trimmed,
	)
	agent := &principalRef{Kind: "agent"}
	if err := row.Scan(&agent.UserID, &agent.Handle, &agent.Name); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	return agent, true, nil
}

func extractMentionPrincipalIDsPostgres(q querier, workspaceID string, content string) ([]string, error) {
	matches := mentionPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		handle := content[match[4]:match[5]]
		principal, ok, err := principalByHandlePostgres(q, workspaceID, handle)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if !containsText(ids, principal.UserID) {
			ids = append(ids, principal.UserID)
		}
	}
	return ids, nil
}

func shouldNotifyAgentPostgres(q querier, workspaceID string, agentID string, meta OperationMeta, fallbackActorID string) (bool, error) {
	originID := strings.TrimSpace(fallbackActorID)
	if strings.TrimSpace(meta.ActorID) != "" {
		actor, err := resolvePrincipalPostgres(q, workspaceID, meta.ActorID, meta.ActorType)
		if err != nil && err != ErrNotFound {
			return false, err
		}
		if actor != nil {
			originID = actor.ID
		}
	}
	if originID == "" {
		return true, nil
	}
	return originID != agentID, nil
}

func daemonExistsPostgres(q querier, workspaceID string, daemonID string) (bool, error) {
	trimmed := strings.TrimSpace(daemonID)
	if trimmed == "" {
		return false, nil
	}
	if !isUUIDString(trimmed) {
		return false, nil
	}
	var exists bool
	err := q.QueryRow(
		`SELECT EXISTS(
			SELECT 1
			  FROM daemons
			 WHERE workspace_id = $1::uuid
			   AND id = $2::uuid
			   AND status <> 'deleted'
		)`,
		workspaceID,
		trimmed,
	).Scan(&exists)
	return exists, err
}

func scanAgent(scanner interface{ Scan(...any) error }) (*Agent, error) {
	agent := &Agent{}
	var lastHeartbeat sql.NullTime
	var lastRunCompleted sql.NullTime
	if err := scanner.Scan(
		&agent.ID,
		&agent.DaemonID,
		&agent.Handle,
		&agent.Name,
		&agent.Role,
		&agent.Kind,
		&agent.SystemPrompt,
		&agent.WorkspaceRoot,
		&agent.CurrentTurnID,
		&agent.SessionID,
		&agent.Status,
		&agent.CurrentTask,
		&agent.CurrentActivity,
		&agent.CurrentRunID,
		&lastHeartbeat,
		&lastRunCompleted,
		&agent.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if lastHeartbeat.Valid {
		agent.LastHeartbeatAt = lastHeartbeat.Time
	}
	if lastRunCompleted.Valid {
		agent.LastRunCompleted = lastRunCompleted.Time
	}
	return agent, nil
}

func listAgentRunsPostgres(q querier, workspaceID string) ([]*AgentRun, error) {
	rows, err := q.Query(
		`SELECT id::text, agent_id::text, agent_handle, agent_name, agent_kind, system_prompt, session_id,
		        workspace_root, working_dir, prompt, status, desired_status, process_id,
		        launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
		        log_tail, error, assigned_task_ref, updated_at
		   FROM agent_runs
		  WHERE workspace_id = $1::uuid
		  ORDER BY updated_at DESC, id ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []*AgentRun{}
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return nil, err
		}
		run.WorkspaceID = workspaceID
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func getAgentRunPostgres(q querier, workspaceID string, runID string) (*AgentRun, error) {
	if !isUUIDString(runID) {
		return nil, ErrNotFound
	}
	row := q.QueryRow(
		`SELECT id::text, agent_id::text, agent_handle, agent_name, agent_kind, system_prompt, session_id,
		        workspace_root, working_dir, prompt, status, desired_status, process_id,
		        launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
		        log_tail, error, assigned_task_ref, updated_at
		   FROM agent_runs
		  WHERE workspace_id = $1::uuid
		    AND id = $2::uuid`,
		workspaceID,
		runID,
	)
	run, err := scanAgentRun(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	run.WorkspaceID = workspaceID
	return run, nil
}

func getAgentRunForUpdatePostgres(tx *sql.Tx, workspaceID string, runID string) (*AgentRun, error) {
	if !isUUIDString(runID) {
		return nil, ErrNotFound
	}
	row := tx.QueryRow(
		`SELECT id::text, agent_id::text, agent_handle, agent_name, agent_kind, system_prompt, session_id,
		        workspace_root, working_dir, prompt, status, desired_status, process_id,
		        launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
		        log_tail, error, assigned_task_ref, updated_at
		   FROM agent_runs
		  WHERE workspace_id = $1::uuid
		    AND id = $2::uuid
		  FOR UPDATE`,
		workspaceID,
		runID,
	)
	run, err := scanAgentRun(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	run.WorkspaceID = workspaceID
	return run, nil
}

func insertAgentRunPostgresTx(tx *sql.Tx, workspaceID string, run *AgentRun) error {
	if run == nil {
		return nil
	}
	logTail, err := json.Marshal(run.LogTail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO agent_runs (
			workspace_id, id, agent_id, agent_handle, agent_name, agent_kind, system_prompt,
			session_id, workspace_root, working_dir, prompt, status, desired_status, process_id,
			launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
			log_tail, error, assigned_task_ref, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19,
			$20::jsonb, $21, $22, $23
		)`,
		workspaceID,
		run.ID,
		run.AgentID,
		run.AgentHandle,
		run.AgentName,
		run.AgentKind,
		run.SystemPrompt,
		run.SessionID,
		run.WorkspaceRoot,
		run.WorkingDir,
		run.Prompt,
		run.Status,
		run.DesiredStatus,
		nullInt(run.ProcessID),
		nullTime(run.LaunchTime),
		nullTime(run.LastHeartbeatAt),
		nullTime(run.CompletedAt),
		nullExitCode(run),
		run.LastMessage,
		string(logTail),
		run.Error,
		run.AssignedTaskRef,
		run.UpdatedAt,
	)
	return err
}

func updateAgentRunPostgresTx(tx *sql.Tx, workspaceID string, run *AgentRun) error {
	if run == nil {
		return nil
	}
	logTail, err := json.Marshal(run.LogTail)
	if err != nil {
		return err
	}
	result, err := tx.Exec(
		`UPDATE agent_runs
		    SET status = $3,
		        desired_status = $4,
		        process_id = $5,
		        launch_time = $6,
		        last_heartbeat_at = $7,
		        completed_at = $8,
		        exit_code = $9,
		        last_message = $10,
		        log_tail = $11::jsonb,
		        error = $12,
		        session_id = $13,
		        updated_at = $14
		  WHERE workspace_id = $1::uuid
		    AND id = $2::uuid`,
		workspaceID,
		run.ID,
		run.Status,
		run.DesiredStatus,
		nullInt(run.ProcessID),
		nullTime(run.LaunchTime),
		nullTime(run.LastHeartbeatAt),
		nullTime(run.CompletedAt),
		nullExitCode(run),
		run.LastMessage,
		string(logTail),
		run.Error,
		run.SessionID,
		run.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return ErrNotFound
	}
	return nil
}

// listActivitiesPostgres returns the newest window of activities for a
// workspace directly from Postgres (source of truth), newest first. The window
// (LIMIT) shapes the read only; rows are never trimmed from the table.
func listActivitiesPostgres(q querier, workspaceID string) ([]*ActivityEvent, error) {
	rows, err := q.Query(
		`SELECT id, type, COALESCE(document_id::text, ''), COALESCE(actor_id::text, ''), actor_type, summary, occurred_at,
		        COALESCE(provenance_actor_id::text, ''), provenance_actor_type, provenance_execution_id,
		        provenance_tool, provenance_trigger, provenance_autonomous,
		        provenance_confidence, provenance_requested_by, provenance_source,
		        provenance_intended_scope, provenance_read_set_summary,
		        presence_ref
		   FROM activities
		  WHERE workspace_id = $1::uuid
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT 100`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	activities := []*ActivityEvent{}
	for rows.Next() {
		activity := &ActivityEvent{}
		if err := rows.Scan(
			&activity.ID,
			&activity.Type,
			&activity.DocumentID,
			&activity.ActorID,
			&activity.ActorType,
			&activity.Summary,
			&activity.OccurredAt,
			&activity.Provenance.ActorID,
			&activity.Provenance.ActorType,
			&activity.Provenance.ExecutionID,
			&activity.Provenance.Tool,
			&activity.Provenance.Trigger,
			&activity.Provenance.Autonomous,
			&activity.Provenance.Confidence,
			&activity.Provenance.RequestedBy,
			&activity.Provenance.Source,
			&activity.Provenance.IntendedScope,
			&activity.Provenance.ReadSetSummary,
			&activity.PresenceRef,
		); err != nil {
			return nil, err
		}
		activities = append(activities, activity)
	}
	return activities, rows.Err()
}

// seedRootDocumentTx bootstraps a workspace's root document inside an existing
// transaction: the documents row, its initial CRDT update, and the document
// head (the head row is required — loadDocumentsPostgresLocked inner-joins it).
// This is the single source of truth for how a root document is born, so a
// workspace and its root commit together and satisfy the deferred
// fk_workspaces_root_document constraint. A checkpoint is a replay snapshot the
// next persist creates on demand, so none is seeded here.
func seedRootDocumentTx(tx *sql.Tx, workspaceID string, rootDocumentID string, now time.Time) error {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(initialClientIDSeed)))
	defer doc.Close()
	update := doc.EncodeStateAsUpdate()
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))

	if _, err := tx.Exec(
		`INSERT INTO documents (workspace_id, id, hidden, client_id_seed, create_client_operation_id, updated_at)
		 VALUES ($1, $2, true, $3, '', $4)`,
		workspaceID, rootDocumentID, int64(initialClientIDSeed), now,
	); err != nil {
		return err
	}

	var updateID int64
	if err := tx.QueryRow(
		`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		workspaceID, rootDocumentID, update, actorUUIDOrNil("system", "system"), "system", now,
	).Scan(&updateID); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		workspaceID, rootDocumentID, stateVector, updateID, now,
	); err != nil {
		return err
	}
	return nil
}

func (s *Store) loadDocumentsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT d.id::text,
		        d.hidden,
		        COALESCE(NULLIF(h.state_vector, ''), checkpoint.state_vector, '') AS state_vector,
		        h.update_id,
		        d.updated_at,
		        d.client_id_seed,
		        d.create_client_operation_id
		   FROM documents d
		   JOIN document_heads h
		     ON h.workspace_id = d.workspace_id AND h.document_id = d.id
		   LEFT JOIN LATERAL (
		       SELECT state_vector
		         FROM document_checkpoints c
		        WHERE c.workspace_id = d.workspace_id
		          AND c.document_id = d.id
		          AND c.update_id <= h.update_id
		        ORDER BY c.update_id DESC
		        LIMIT 1
		   ) checkpoint ON TRUE
		  WHERE d.workspace_id = $1::uuid
		  ORDER BY d.id ASC`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		document := &Document{}
		var clientIDSeed int64
		if err := rows.Scan(
			&document.ID,
			&document.Hidden,
			&document.StateVector,
			&document.UpdateID,
			&document.UpdatedAt,
			&clientIDSeed,
			&document.CreateClientOperationID,
		); err != nil {
			return err
		}
		document.ClientIDSeed = uint64(clientIDSeed)
		s.state.ContentDocuments[document.ID] = document
	}
	return rows.Err()
}

func (s *Store) restoreDocumentDocPostgresLocked(document *Document) (*crdt.Doc, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(document.ClientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	var checkpointUpdateID int64
	err := s.db.QueryRow(
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id = $1::uuid AND document_id = $2::uuid AND update_id <= $3
		  ORDER BY update_id DESC
		  LIMIT 1`,
		s.state.WorkspaceID,
		document.ID,
		document.UpdateID,
	).Scan(&checkpointUpdateID, &checkpointState)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil && checkpointState != "" {
		update, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if applyErr := crdt.ApplyUpdateV1(doc, update, "checkpoint"); applyErr != nil {
			return nil, applyErr
		}
		appliedThrough = checkpointUpdateID
	}
	if appliedThrough > document.UpdateID {
		appliedThrough = 0
	}
	if appliedThrough >= document.UpdateID {
		return doc, nil
	}
	updateRows, err := s.db.Query(
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id = $1::uuid
		    AND document_id = $2::uuid
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		s.state.WorkspaceID,
		document.ID,
		appliedThrough,
		document.UpdateID,
	)
	if err != nil {
		return nil, err
	}
	defer updateRows.Close()
	for updateRows.Next() {
		var update []byte
		if err := updateRows.Scan(&update); err != nil {
			return nil, err
		}
		if err := crdt.ApplyUpdateV1(doc, update, "restore-update"); err != nil {
			return nil, err
		}
	}
	if err := updateRows.Err(); err != nil {
		return nil, err
	}
	return doc, nil
}

func scanAgentRun(scanner interface{ Scan(...any) error }) (*AgentRun, error) {
	run := &AgentRun{}
	var processID sql.NullInt64
	var launchTime sql.NullTime
	var lastHeartbeat sql.NullTime
	var completedAt sql.NullTime
	var exitCode sql.NullInt64
	var logTailRaw []byte
	if err := scanner.Scan(
		&run.ID,
		&run.AgentID,
		&run.AgentHandle,
		&run.AgentName,
		&run.AgentKind,
		&run.SystemPrompt,
		&run.SessionID,
		&run.WorkspaceRoot,
		&run.WorkingDir,
		&run.Prompt,
		&run.Status,
		&run.DesiredStatus,
		&processID,
		&launchTime,
		&lastHeartbeat,
		&completedAt,
		&exitCode,
		&run.LastMessage,
		&logTailRaw,
		&run.Error,
		&run.AssignedTaskRef,
		&run.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if processID.Valid {
		run.ProcessID = int(processID.Int64)
	}
	if launchTime.Valid {
		run.LaunchTime = launchTime.Time
	}
	if lastHeartbeat.Valid {
		run.LastHeartbeatAt = lastHeartbeat.Time
	}
	if completedAt.Valid {
		run.CompletedAt = completedAt.Time
	}
	if exitCode.Valid {
		run.ExitCode = int(exitCode.Int64)
	}
	run.LogTail = []string{}
	if len(logTailRaw) > 0 {
		if err := json.Unmarshal(logTailRaw, &run.LogTail); err != nil {
			return nil, err
		}
	}
	return run, nil
}

func scanThread(scanner interface{ Scan(...any) error }) (*Thread, error) {
	thread := &Thread{Messages: []*ThreadMessage{}, ParticipantIDs: []string{}, ParticipantHandles: []string{}}
	if err := scanner.Scan(
		&thread.ID,
		&thread.DocumentID,
		&thread.ClientOperationID,
		&thread.Title,
		&thread.Status,
		&thread.Anchor.RelativeStart,
		&thread.Anchor.RelativeEnd,
		&thread.Anchor.Kind,
		&thread.Anchor.Excerpt,
		&thread.CreatedByID,
		&thread.CreatedByType,
		&thread.CreatedByHandle,
		&thread.CreatedByName,
		&thread.CreatedAt,
		&thread.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return thread, nil
}

func scanThreadMessage(scanner interface{ Scan(...any) error }) (*ThreadMessage, string, error) {
	message := &ThreadMessage{}
	var threadID string
	if err := scanner.Scan(
		&message.ID,
		&threadID,
		&message.AuthorID,
		&message.AuthorType,
		&message.AuthorHandle,
		&message.AuthorName,
		&message.Body,
		&message.Kind,
		&message.CreatedAt,
	); err != nil {
		return nil, "", err
	}
	message.ThreadID = threadID
	return message, threadID, nil
}

func scanAgentEvent(scanner interface{ Scan(...any) error }) (*AgentEvent, error) {
	event := &AgentEvent{}
	var claimedAt sql.NullTime
	var completedAt sql.NullTime
	if err := scanner.Scan(
		&event.ID,
		&event.AgentID,
		&event.AgentHandle,
		&event.Type,
		&event.Box,
		&event.Status,
		&event.DocumentID,
		&event.ThreadID,
		&event.ThreadMessageID,
		&event.FromUpdateID,
		&event.ToUpdateID,
		&event.Summary,
		&event.Prompt,
		&event.DedupKey,
		&event.ClaimedBy,
		&event.RunID,
		&event.LastError,
		&event.AttemptCount,
		&event.AvailableAt,
		&claimedAt,
		&completedAt,
		&event.CreatedAt,
		&event.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if claimedAt.Valid {
		event.ClaimedAt = claimedAt.Time
	}
	if completedAt.Valid {
		event.CompletedAt = completedAt.Time
	}
	event.Box = normalizeInboxBox(event.Box)
	return event, nil
}

func stringsOr(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func selectionBounds(selection []int) (any, any) {
	if len(selection) == 0 {
		return nil, nil
	}
	start := selection[0]
	if len(selection) == 1 {
		return start, start
	}
	return start, selection[1]
}

func selectionFromNulls(start, end sql.NullInt64) []int {
	if !start.Valid && !end.Valid {
		return []int{}
	}
	if start.Valid && end.Valid {
		return []int{int(start.Int64), int(end.Int64)}
	}
	if start.Valid {
		return []int{int(start.Int64)}
	}
	return []int{int(end.Int64)}
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func nullInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullExitCode(run *AgentRun) any {
	if run == nil {
		return nil
	}
	if run.CompletedAt.IsZero() && run.ExitCode == 0 {
		return nil
	}
	return run.ExitCode
}

func listThreadsPostgres(q querier, workspaceID string) ([]*Thread, error) {
	return queryThreadsPostgres(q, workspaceID, "", "")
}

func listThreadsForDocumentPostgres(q querier, workspaceID string, documentID string) ([]*Thread, error) {
	return queryThreadsPostgres(q, workspaceID, documentID, "")
}

func getThreadPostgres(q querier, workspaceID string, threadID string) (*Thread, error) {
	threads, err := queryThreadsPostgres(q, workspaceID, "", threadID)
	if err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return nil, ErrNotFound
	}
	return threads[0], nil
}

func queryThreadsPostgres(q querier, workspaceID string, documentID string, threadID string) ([]*Thread, error) {
	threadQuery := `SELECT t.id::text, t.document_id::text, t.client_operation_id, t.title, t.status,
	       t.anchor_relative_start, t.anchor_relative_end, t.anchor_kind, t.anchor_excerpt,
	       COALESCE(t.created_by_id::text, ''), t.created_by_type,
	       COALESCE(u.handle, a.handle, t.created_by_handle) AS created_by_handle,
	       COALESCE(u.name, a.name, t.created_by_name) AS created_by_name,
	       t.created_at, t.updated_at
	  FROM threads t
	  LEFT JOIN users u ON u.id = t.created_by_id AND u.workspace_id = t.workspace_id
	  LEFT JOIN agents a ON a.id = t.created_by_id AND a.workspace_id = t.workspace_id
	 WHERE t.workspace_id = $1::uuid`
	args := []any{workspaceID}
	if documentID != "" {
		threadQuery += ` AND t.document_id = $2::uuid`
		args = append(args, documentID)
	}
	if threadID != "" {
		threadQuery += fmt.Sprintf(` AND t.id = $%d::uuid`, len(args)+1)
		args = append(args, threadID)
	}
	threadQuery += ` ORDER BY t.updated_at DESC, t.id ASC`

	rows, err := q.Query(threadQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threadsByID := map[string]*Thread{}
	threads := make([]*Thread, 0)
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		threadsByID[thread.ID] = thread
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return threads, nil
	}

	participantQuery := `SELECT tp.thread_id::text, tp.participant_id::text,
	       COALESCE(u.handle, a.handle, '') AS participant_handle
	  FROM thread_participants tp
	  LEFT JOIN users u ON u.id = tp.participant_id AND u.workspace_id = tp.workspace_id
	  LEFT JOIN agents a ON a.id = tp.participant_id AND a.workspace_id = tp.workspace_id
	 WHERE tp.workspace_id = $1::uuid`
	pArgs := []any{workspaceID}
	if documentID != "" {
		participantQuery += ` AND tp.thread_id IN (SELECT id FROM threads WHERE workspace_id = $1::uuid AND document_id = $2::uuid)`
		pArgs = append(pArgs, documentID)
	}
	if threadID != "" {
		participantQuery += fmt.Sprintf(` AND tp.thread_id = $%d::uuid`, len(pArgs)+1)
		pArgs = append(pArgs, threadID)
	}
	participantQuery += ` ORDER BY tp.thread_id, tp.participant_id`

	pRows, err := q.Query(participantQuery, pArgs...)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var tid, pid, phandle string
		if err := pRows.Scan(&tid, &pid, &phandle); err != nil {
			return nil, err
		}
		thread := threadsByID[tid]
		if thread == nil {
			continue
		}
		thread.ParticipantIDs = append(thread.ParticipantIDs, pid)
		if phandle != "" {
			thread.ParticipantHandles = append(thread.ParticipantHandles, phandle)
		}
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}
	for _, thread := range threads {
		sort.Strings(thread.ParticipantHandles)
	}

	messageQuery := `SELECT m.id::text, m.thread_id::text, COALESCE(m.author_id::text, ''), m.author_type,
	       COALESCE(u.handle, a.handle, m.author_handle) AS author_handle,
	       COALESCE(u.name, a.name, m.author_name) AS author_name,
	       m.body, m.kind, m.created_at
	  FROM thread_messages m
	  LEFT JOIN users u ON u.id = m.author_id AND u.workspace_id = m.workspace_id
	  LEFT JOIN agents a ON a.id = m.author_id AND a.workspace_id = m.workspace_id
	 WHERE m.workspace_id = $1::uuid`
	mArgs := []any{workspaceID}
	if documentID != "" {
		messageQuery += ` AND m.thread_id IN (SELECT id FROM threads WHERE workspace_id = $1::uuid AND document_id = $2::uuid)`
		mArgs = append(mArgs, documentID)
	}
	if threadID != "" {
		messageQuery += fmt.Sprintf(` AND m.thread_id = $%d::uuid`, len(mArgs)+1)
		mArgs = append(mArgs, threadID)
	}
	messageQuery += ` ORDER BY m.created_at ASC, m.id ASC`

	mRows, err := q.Query(messageQuery, mArgs...)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()
	for mRows.Next() {
		message, tid, err := scanThreadMessage(mRows)
		if err != nil {
			return nil, err
		}
		thread := threadsByID[tid]
		if thread == nil {
			continue
		}
		thread.Messages = append(thread.Messages, message)
	}
	if err := mRows.Err(); err != nil {
		return nil, err
	}

	return threads, nil
}

func createThreadPostgres(db *sql.DB, workspaceID string, thread *Thread, message *ThreadMessage, events []*AgentEvent, activity *ActivityEvent) (*Thread, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.Exec(
		`INSERT INTO threads (
			workspace_id, id, document_id, client_operation_id, title, status,
			anchor_relative_start, anchor_relative_end, anchor_kind, anchor_excerpt,
			created_by_id, created_by_type, created_by_handle, created_by_name,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16
		)
		ON CONFLICT (workspace_id, created_by_id, created_by_type, client_operation_id)
			WHERE client_operation_id <> ''
		DO NOTHING`,
		workspaceID,
		thread.ID,
		thread.DocumentID,
		thread.ClientOperationID,
		thread.Title,
		thread.Status,
		thread.Anchor.RelativeStart,
		thread.Anchor.RelativeEnd,
		thread.Anchor.Kind,
		thread.Anchor.Excerpt,
		uuidStringOrNil(thread.CreatedByID),
		thread.CreatedByType,
		thread.CreatedByHandle,
		thread.CreatedByName,
		thread.CreatedAt,
		thread.UpdatedAt,
	)
	if err != nil {
		return nil, false, err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		_ = tx.Rollback()
		return nil, false, nil
	}

	for _, participantID := range thread.ParticipantIDs {
		if _, err = tx.Exec(
			`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (workspace_id, thread_id, participant_id) DO NOTHING`,
			workspaceID, thread.ID, participantID,
		); err != nil {
			return nil, false, err
		}
	}

	if message != nil {
		if _, err = tx.Exec(
			`INSERT INTO thread_messages (
				workspace_id, id, thread_id, author_id, author_type,
				author_handle, author_name, body, kind, created_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10
			)`,
			workspaceID,
			message.ID,
			thread.ID,
			uuidStringOrNil(message.AuthorID),
			message.AuthorType,
			message.AuthorHandle,
			message.AuthorName,
			message.Body,
			message.Kind,
			message.CreatedAt,
		); err != nil {
			return nil, false, err
		}
	}

	for _, event := range events {
		if err = insertAgentEventTx(tx, workspaceID, event); err != nil {
			return nil, false, err
		}
	}

	if err = insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, false, err
	}

	committed, err := getThreadPostgres(tx, workspaceID, thread.ID)
	if err != nil {
		return nil, false, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, false, err
	}
	return committed, true, nil
}

func replyThreadPostgres(db *sql.DB, workspaceID string, threadID string, message *ThreadMessage, meta OperationMeta) (*Thread, []*AgentEvent, *ActivityEvent, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	now := message.CreatedAt
	var foundID string
	err = tx.QueryRow(
		`UPDATE threads SET updated_at = $1 WHERE workspace_id = $2::uuid AND id = $3::uuid RETURNING id::text`,
		now, workspaceID, threadID,
	).Scan(&foundID)
	if err == sql.ErrNoRows {
		_ = tx.Rollback()
		return nil, nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}

	if _, err = tx.Exec(
		`INSERT INTO thread_messages (
			workspace_id, id, thread_id, author_id, author_type,
			author_handle, author_name, body, kind, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10
		)`,
		workspaceID,
		message.ID,
		threadID,
		uuidStringOrNil(message.AuthorID),
		message.AuthorType,
		message.AuthorHandle,
		message.AuthorName,
		message.Body,
		message.Kind,
		message.CreatedAt,
	); err != nil {
		return nil, nil, nil, err
	}

	mentionedIDs, err := extractMentionPrincipalIDsPostgres(tx, workspaceID, message.Body)
	if err != nil {
		return nil, nil, nil, err
	}
	participantIDs := append([]string{message.AuthorID}, mentionedIDs...)
	for _, pid := range participantIDs {
		if _, err = tx.Exec(
			`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (workspace_id, thread_id, participant_id) DO NOTHING`,
			workspaceID, threadID, pid,
		); err != nil {
			return nil, nil, nil, err
		}
	}

	thread, err := getThreadPostgres(tx, workspaceID, threadID)
	if err != nil {
		return nil, nil, nil, err
	}
	mentionEvents, err := collectThreadMentionEventsPostgres(tx, workspaceID, thread, message, meta)
	if err != nil {
		return nil, nil, nil, err
	}
	replyEvents, err := collectThreadReplyEventsPostgres(tx, workspaceID, thread, message, meta, mentionedIDs...)
	if err != nil {
		return nil, nil, nil, err
	}
	events := append(mentionEvents, replyEvents...)
	for _, event := range events {
		if err = insertAgentEventTx(tx, workspaceID, event); err != nil {
			return nil, nil, nil, err
		}
	}

	activity := &ActivityEvent{
		Type:       "thread.replied",
		DocumentID: thread.DocumentID,
		ActorID:    meta.ActorID,
		ActorType:  meta.ActorType,
		Summary:    fmt.Sprintf("%s replied in thread %s", meta.ActorID, thread.Title),
		OccurredAt: now,
		Provenance: meta,
	}
	if err = insertActivityPostgres(tx, workspaceID, activity); err != nil {
		return nil, nil, nil, err
	}

	result, err := getThreadPostgres(tx, workspaceID, threadID)
	if err != nil {
		return nil, nil, nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, nil, nil, err
	}
	return result, events, activity, nil
}

func updateThreadStatusPostgres(db *sql.DB, workspaceID string, threadID string, status string) (*Thread, bool, error) {
	var id string
	var priorStatus string
	err := db.QueryRow(
		`WITH prior AS (
			SELECT status FROM threads WHERE workspace_id = $3::uuid AND id = $4::uuid
		)
		UPDATE threads t
		   SET status = $1,
		       updated_at = CASE WHEN t.status IS DISTINCT FROM $1 THEN $2 ELSE t.updated_at END
		 WHERE t.workspace_id = $3::uuid AND t.id = $4::uuid
		 RETURNING t.id::text, (SELECT status FROM prior)`,
		status, time.Now().UTC(), workspaceID, threadID,
	).Scan(&id, &priorStatus)
	if err == sql.ErrNoRows {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	thread, err := getThreadPostgres(db, workspaceID, threadID)
	if err != nil {
		return nil, false, err
	}
	return thread, priorStatus != status, nil
}

func updateThreadAnchorPostgres(db *sql.DB, workspaceID string, threadID string, req UpdateThreadAnchorRequest) (*Thread, bool, error) {
	existing, err := getThreadPostgres(db, workspaceID, threadID)
	if err != nil {
		return nil, false, err
	}
	// Omitted excerpt preserves the stored one; a provided value (including "") replaces it.
	excerpt := existing.Anchor.Excerpt
	if req.Excerpt != nil {
		excerpt = *req.Excerpt
	}
	anchor, err := buildThreadAnchor(req.Kind, req.RelativeStart, req.RelativeEnd, excerpt)
	if err != nil {
		return nil, false, err
	}
	if anchor == existing.Anchor {
		// Idempotent no-op: identical anchor, no write and no broadcast.
		return existing, false, nil
	}
	if _, err := db.Exec(
		`UPDATE threads
		    SET anchor_kind = $1,
		        anchor_relative_start = $2,
		        anchor_relative_end = $3,
		        anchor_excerpt = $4,
		        updated_at = $5
		  WHERE workspace_id = $6::uuid AND id = $7::uuid`,
		anchor.Kind, anchor.RelativeStart, anchor.RelativeEnd, anchor.Excerpt, time.Now().UTC(), workspaceID, threadID,
	); err != nil {
		return nil, false, err
	}
	thread, err := getThreadPostgres(db, workspaceID, threadID)
	if err != nil {
		return nil, false, err
	}
	return thread, true, nil
}

func findThreadByClientOperationPostgres(db *sql.DB, workspaceID string, clientOperationID string, createdByID string, createdByType string) (*Thread, error) {
	var threadID string
	err := db.QueryRow(
		`SELECT id::text FROM threads
		  WHERE workspace_id = $1::uuid AND client_operation_id = $2 AND created_by_id = $3::uuid AND created_by_type = $4
		  LIMIT 1`,
		workspaceID, clientOperationID, createdByID, createdByType,
	).Scan(&threadID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return getThreadPostgres(db, workspaceID, threadID)
}

func documentHasOpenThreadForParticipantPostgres(q querier, workspaceID string, documentID string, participantID string) (bool, string, string, error) {
	var threadID, threadTitle string
	err := q.QueryRow(
		`SELECT t.id::text, t.title
		   FROM threads t
		   JOIN thread_participants tp ON tp.workspace_id = t.workspace_id AND tp.thread_id = t.id
		  WHERE t.workspace_id = $1::uuid
		    AND t.document_id = $2::uuid
		    AND t.status = 'open'
		    AND tp.participant_id = $3::uuid
		  LIMIT 1`,
		workspaceID, documentID, participantID,
	).Scan(&threadID, &threadTitle)
	if err == sql.ErrNoRows {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	return true, threadID, threadTitle, nil
}
