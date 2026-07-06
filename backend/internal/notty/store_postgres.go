package notty

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	crdt "notty/internal/ycrdt"
)

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
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS root_document_id UUID`,
		`ALTER TABLE workspaces DROP COLUMN IF EXISTS root_stream_id`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE accounts ALTER COLUMN email_verified SET DEFAULT FALSE`,
		`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_accessed_workspace_id UUID`,
		`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS password_updated_at TIMESTAMPTZ`,
		`
		CREATE TABLE IF NOT EXISTS account_email_tokens (
			id UUID PRIMARY KEY,
			account_id UUID NOT NULL,
			purpose TEXT NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_account_email_tokens_account_purpose_created ON account_email_tokens (account_id, purpose, created_at DESC)`,
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
			PRIMARY KEY (workspace_id, account_id)
		)
		`,
		`ALTER TABLE workspace_members ADD COLUMN IF NOT EXISTS last_accessed_document_id UUID`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_members_account ON workspace_members (account_id, status, workspace_id)`,
		`
		CREATE TABLE IF NOT EXISTS workspace_invites (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			token_hash TEXT UNIQUE NOT NULL,
			created_by_user_id UUID NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_invites_workspace ON workspace_invites (workspace_id)`,
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
			deleted_at TIMESTAMPTZ
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_daemons_workspace ON daemons (workspace_id, status)`,
		`ALTER TABLE daemons ADD COLUMN IF NOT EXISTS daemon_version TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE daemons ADD COLUMN IF NOT EXISTS os TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE daemons ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE daemons ADD COLUMN IF NOT EXISTS runtime_detections JSONB NOT NULL DEFAULT '[]'::jsonb`,
		`ALTER TABLE daemons DROP COLUMN IF EXISTS pending_token_hash`,
		`
		CREATE TABLE IF NOT EXISTS documents (
			workspace_id UUID NOT NULL,
			id UUID PRIMARY KEY,
			path TEXT NOT NULL,
			title TEXT NOT NULL,
			hidden BOOLEAN NOT NULL DEFAULT false,
			client_id_seed BIGINT NOT NULL,
			create_client_operation_id TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS create_client_operation_id TEXT NOT NULL DEFAULT ''`,
		`DROP INDEX IF EXISTS idx_documents_workspace_path`,
		`DROP INDEX IF EXISTS idx_documents_workspace_visible_path`,
		`
		CREATE TABLE IF NOT EXISTS document_heads (
			workspace_id UUID NOT NULL,
			document_id UUID PRIMARY KEY,
			state_vector TEXT NOT NULL DEFAULT '',
			update_id BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE document_heads ADD COLUMN IF NOT EXISTS state_vector TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE document_heads ADD COLUMN IF NOT EXISTS update_id BIGINT NOT NULL DEFAULT 0`,
		`
		CREATE TABLE IF NOT EXISTS document_updates (
			id BIGSERIAL PRIMARY KEY,
			workspace_id UUID NOT NULL,
			document_id UUID NOT NULL,
			update BYTEA NOT NULL,
			actor_id UUID,
			actor_type TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_created ON document_updates (workspace_id, document_id, created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_id ON document_updates (workspace_id, document_id, id ASC)`,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM information_schema.tables
				 WHERE table_schema = 'public' AND table_name = 'document_checkpoints'
			) THEN
				IF EXISTS (
					SELECT 1 FROM information_schema.columns
					 WHERE table_schema = 'public'
					   AND table_name = 'document_heads'
					   AND column_name = 'workspace_id'
					   AND data_type = 'text'
				) THEN
					EXECUTE '
						CREATE TABLE document_checkpoints (
							id BIGSERIAL PRIMARY KEY,
							workspace_id TEXT NOT NULL,
							document_id TEXT NOT NULL,
							update_id BIGINT NOT NULL,
							crdt_state TEXT NOT NULL,
							state_vector TEXT NOT NULL,
							created_at TIMESTAMPTZ NOT NULL,
							UNIQUE (workspace_id, document_id, update_id)
						)';
				ELSE
					EXECUTE '
						CREATE TABLE document_checkpoints (
							id BIGSERIAL PRIMARY KEY,
							workspace_id UUID NOT NULL,
							document_id UUID NOT NULL,
							update_id BIGINT NOT NULL,
							crdt_state TEXT NOT NULL,
							state_vector TEXT NOT NULL,
							created_at TIMESTAMPTZ NOT NULL,
							UNIQUE (workspace_id, document_id, update_id)
						)';
				END IF;
			END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_document_checkpoints_workspace_document_update ON document_checkpoints (workspace_id, document_id, update_id DESC)`,
		`UPDATE document_heads h
		    SET update_id = COALESCE((
			    SELECT MAX(u.id)
			      FROM document_updates u
			     WHERE u.workspace_id::text = h.workspace_id::text AND u.document_id = h.document_id
		    ), 0)
		  WHERE h.update_id = 0`,
		`DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_name = 'document_heads' AND column_name = 'crdt_state'
			) THEN
				IF EXISTS (
					SELECT 1 FROM information_schema.columns
					 WHERE table_name = 'document_heads' AND column_name = 'crdt_state_update_id'
				) THEN
					EXECUTE '
						INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
						SELECT workspace_id,
						       document_id,
						       CASE WHEN crdt_state_update_id > 0 THEN crdt_state_update_id ELSE update_id END,
						       crdt_state,
						       state_vector,
						       updated_at
						  FROM document_heads
						 WHERE crdt_state <> ''''
						   AND CASE WHEN crdt_state_update_id > 0 THEN crdt_state_update_id ELSE update_id END > 0
						ON CONFLICT (workspace_id, document_id, update_id) DO NOTHING';
				ELSE
					EXECUTE '
						INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector, created_at)
						SELECT workspace_id, document_id, update_id, crdt_state, state_vector, updated_at
						  FROM document_heads
						 WHERE crdt_state <> '''' AND update_id > 0
						ON CONFLICT (workspace_id, document_id, update_id) DO NOTHING';
				END IF;
			END IF;
		END $$`,
		`ALTER TABLE document_heads DROP COLUMN IF EXISTS content`,
		`ALTER TABLE document_heads DROP COLUMN IF EXISTS crdt_state`,
		`ALTER TABLE document_heads DROP COLUMN IF EXISTS crdt_state_update_id`,
		`DROP TABLE IF EXISTS document_mentions`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE users DROP COLUMN IF EXISTS daemon_id`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_workspace_handle ON users (workspace_id, handle)`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_workspace_handle ON agents (workspace_id, handle)`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS daemon_id UUID`,
		`ALTER TABLE agents DROP COLUMN IF EXISTS codex_thread_id`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS current_turn_id TEXT NOT NULL DEFAULT ''`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_agent_runs_workspace_agent_updated ON agent_runs (workspace_id, agent_id, updated_at DESC)`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT ''`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS client_operation_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ALTER COLUMN created_by_id DROP NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_threads_workspace_document_updated ON threads (workspace_id, document_id, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_workspace_actor_operation ON threads (workspace_id, created_by_id, created_by_type, client_operation_id) WHERE client_operation_id <> ''`,
		`DROP TABLE IF EXISTS workspace_snapshots`,
		`ALTER TABLE threads DROP COLUMN IF EXISTS anchor`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_relative_start TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_relative_end TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads DROP COLUMN IF EXISTS anchor_start`,
		`ALTER TABLE threads DROP COLUMN IF EXISTS anchor_end`,
		`ALTER TABLE threads DROP COLUMN IF EXISTS anchor_line`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_excerpt TEXT NOT NULL DEFAULT ''`,
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
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE thread_messages ALTER COLUMN author_id DROP NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_thread_messages_workspace_thread_created ON thread_messages (workspace_id, thread_id, created_at ASC)`,
		`
		CREATE TABLE IF NOT EXISTS thread_participants (
			workspace_id UUID NOT NULL,
			thread_id UUID NOT NULL,
			participant_id UUID NOT NULL,
			PRIMARY KEY (workspace_id, thread_id, participant_id)
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS presences (
			workspace_id UUID NOT NULL,
			actor_id UUID NOT NULL,
			actor_type TEXT NOT NULL,
			document_id UUID NOT NULL,
			file_path TEXT NOT NULL,
			mode TEXT NOT NULL,
			selection_start INTEGER,
			selection_end INTEGER,
			activity TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, actor_id)
		)
		`,
		`DROP TABLE IF EXISTS proposals`,
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
				comment_id TEXT NOT NULL DEFAULT '',
				presence_ref TEXT NOT NULL
			)
			`,
		`CREATE INDEX IF NOT EXISTS idx_activities_workspace_occurred ON activities (workspace_id, occurred_at DESC, id DESC)`,
		`ALTER TABLE activities DROP COLUMN IF EXISTS proposal_id`,
		`ALTER TABLE activities ALTER COLUMN comment_id SET DEFAULT ''`,
		`ALTER TABLE activities DROP COLUMN IF EXISTS new_content`,
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
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE agent_events ADD COLUMN IF NOT EXISTS box TEXT NOT NULL DEFAULT 'for_me'`,
		`ALTER TABLE agent_events DROP COLUMN IF EXISTS anchor_start`,
		`ALTER TABLE agent_events DROP COLUMN IF EXISTS anchor_end`,
		`ALTER TABLE agent_events ADD COLUMN IF NOT EXISTS from_update_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_events ADD COLUMN IF NOT EXISTS to_update_id BIGINT NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_agent_events_workspace_agent_claim ON agent_events (workspace_id, agent_id, status, available_at, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_events_workspace_agent_box_status ON agent_events (workspace_id, agent_id, box, status, created_at)`,
		`
		CREATE TABLE IF NOT EXISTS agent_document_views (
			workspace_id UUID NOT NULL,
			agent_id UUID NOT NULL,
			document_id UUID NOT NULL,
			update_id BIGINT NOT NULL,
			state_vector TEXT NOT NULL,
			viewed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, agent_id, document_id)
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

// initPostgresSchemaConstraints adds FK constraints, composite unique indexes,
// and constraint triggers after the native UUID tables exist.
func initPostgresSchemaConstraints(db *sql.DB) error {
	statements := []string{
		// ── Composite unique indexes for same-workspace enforcement ──

		`CREATE UNIQUE INDEX IF NOT EXISTS uq_documents_workspace_id ON documents (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_users_workspace_id ON users (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_workspace_id ON agents (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_daemons_workspace_id ON daemons (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_threads_workspace_id ON threads (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_thread_messages_workspace_id ON thread_messages (workspace_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_runs_workspace_id ON agent_runs (workspace_id, id)`,

		// ── Workspace ownership (CASCADE) ──

		`DO $$ BEGIN ALTER TABLE workspace_members ADD CONSTRAINT fk_workspace_members_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_members VALIDATE CONSTRAINT fk_workspace_members_workspace`,

		`DO $$ BEGIN ALTER TABLE workspace_invites ADD CONSTRAINT fk_workspace_invites_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_invites VALIDATE CONSTRAINT fk_workspace_invites_workspace`,

		`DO $$ BEGIN ALTER TABLE daemons ADD CONSTRAINT fk_daemons_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE daemons VALIDATE CONSTRAINT fk_daemons_workspace`,

		`DO $$ BEGIN ALTER TABLE documents ADD CONSTRAINT fk_documents_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE documents VALIDATE CONSTRAINT fk_documents_workspace`,

		`DO $$ BEGIN ALTER TABLE document_heads ADD CONSTRAINT fk_document_heads_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_heads VALIDATE CONSTRAINT fk_document_heads_workspace`,

		`DO $$ BEGIN ALTER TABLE document_updates ADD CONSTRAINT fk_document_updates_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_updates VALIDATE CONSTRAINT fk_document_updates_workspace`,

		`DO $$ BEGIN ALTER TABLE document_checkpoints ADD CONSTRAINT fk_document_checkpoints_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_checkpoints VALIDATE CONSTRAINT fk_document_checkpoints_workspace`,

		`DO $$ BEGIN ALTER TABLE users ADD CONSTRAINT fk_users_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE users VALIDATE CONSTRAINT fk_users_workspace`,

		`DO $$ BEGIN ALTER TABLE agents ADD CONSTRAINT fk_agents_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agents VALIDATE CONSTRAINT fk_agents_workspace`,

		`DO $$ BEGIN ALTER TABLE agent_runs ADD CONSTRAINT fk_agent_runs_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_runs VALIDATE CONSTRAINT fk_agent_runs_workspace`,

		`DO $$ BEGIN ALTER TABLE threads ADD CONSTRAINT fk_threads_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE threads VALIDATE CONSTRAINT fk_threads_workspace`,

		`DO $$ BEGIN ALTER TABLE thread_messages ADD CONSTRAINT fk_thread_messages_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE thread_messages VALIDATE CONSTRAINT fk_thread_messages_workspace`,

		`DO $$ BEGIN ALTER TABLE thread_participants ADD CONSTRAINT fk_thread_participants_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE thread_participants VALIDATE CONSTRAINT fk_thread_participants_workspace`,

		`DO $$ BEGIN ALTER TABLE presences ADD CONSTRAINT fk_presences_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE presences VALIDATE CONSTRAINT fk_presences_workspace`,

		`DO $$ BEGIN ALTER TABLE activities ADD CONSTRAINT fk_activities_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE activities VALIDATE CONSTRAINT fk_activities_workspace`,

		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_workspace`,

		`DO $$ BEGIN ALTER TABLE agent_document_views ADD CONSTRAINT fk_agent_document_views_workspace FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_document_views VALIDATE CONSTRAINT fk_agent_document_views_workspace`,

		// ── Account/auth ──

		`DO $$ BEGIN ALTER TABLE account_email_tokens ADD CONSTRAINT fk_account_email_tokens_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE account_email_tokens VALIDATE CONSTRAINT fk_account_email_tokens_account`,

		`DO $$ BEGIN ALTER TABLE accounts ADD CONSTRAINT fk_accounts_last_workspace FOREIGN KEY (last_accessed_workspace_id) REFERENCES workspaces(id) ON DELETE SET NULL NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE accounts VALIDATE CONSTRAINT fk_accounts_last_workspace`,

		`DO $$ BEGIN ALTER TABLE workspace_members ADD CONSTRAINT fk_workspace_members_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_members VALIDATE CONSTRAINT fk_workspace_members_account`,

		// ── Membership/invite (composite same-workspace enforcement) ──

		`DO $$ BEGIN ALTER TABLE workspace_members ADD CONSTRAINT fk_workspace_members_user FOREIGN KEY (workspace_id, user_id) REFERENCES users(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_members VALIDATE CONSTRAINT fk_workspace_members_user`,

		`DO $$ BEGIN ALTER TABLE workspace_members ADD CONSTRAINT fk_workspace_members_invited_by FOREIGN KEY (workspace_id, invited_by) REFERENCES users(workspace_id, id) ON DELETE SET NULL (invited_by) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_members VALIDATE CONSTRAINT fk_workspace_members_invited_by`,

		`DO $$ BEGIN ALTER TABLE workspace_invites ADD CONSTRAINT fk_workspace_invites_created_by FOREIGN KEY (workspace_id, created_by_user_id) REFERENCES users(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_invites VALIDATE CONSTRAINT fk_workspace_invites_created_by`,

		// ── Daemon/agent/run (composite same-workspace enforcement) ──

		`DO $$ BEGIN ALTER TABLE agents ADD CONSTRAINT fk_agents_daemon FOREIGN KEY (workspace_id, daemon_id) REFERENCES daemons(workspace_id, id) ON DELETE SET NULL (daemon_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agents VALIDATE CONSTRAINT fk_agents_daemon`,

		`DO $$ BEGIN ALTER TABLE agent_runs ADD CONSTRAINT fk_agent_runs_agent FOREIGN KEY (workspace_id, agent_id) REFERENCES agents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_runs VALIDATE CONSTRAINT fk_agent_runs_agent`,

		`DO $$ BEGIN ALTER TABLE agents ADD CONSTRAINT fk_agents_current_run FOREIGN KEY (workspace_id, current_run_id) REFERENCES agent_runs(workspace_id, id) ON DELETE SET NULL (current_run_id) DEFERRABLE INITIALLY DEFERRED NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agents VALIDATE CONSTRAINT fk_agents_current_run`,

		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_agent FOREIGN KEY (workspace_id, agent_id) REFERENCES agents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_agent`,

		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_run FOREIGN KEY (workspace_id, run_id) REFERENCES agent_runs(workspace_id, id) ON DELETE SET NULL (run_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_run`,

		`DO $$ BEGIN ALTER TABLE agent_document_views ADD CONSTRAINT fk_agent_document_views_agent FOREIGN KEY (workspace_id, agent_id) REFERENCES agents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_document_views VALIDATE CONSTRAINT fk_agent_document_views_agent`,

		// ── Documents (composite same-workspace enforcement) ──

		// NOTE: workspaces(id, root_document_id) -> documents(workspace_id, id) FK is
		// deferred to a future phase. The current workspace creation flow inserts the
		// workspace row and creates the root document in a separate step (ensureRootDocument),
		// so a FK constraint would fail even with DEFERRABLE INITIALLY DEFERRED since the
		// document is created outside the workspace insert transaction.

		`DO $$ BEGIN ALTER TABLE document_heads ADD CONSTRAINT fk_document_heads_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_heads VALIDATE CONSTRAINT fk_document_heads_document`,

		`DO $$ BEGIN ALTER TABLE document_updates ADD CONSTRAINT fk_document_updates_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_updates VALIDATE CONSTRAINT fk_document_updates_document`,

		`DO $$ BEGIN ALTER TABLE document_checkpoints ADD CONSTRAINT fk_document_checkpoints_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE document_checkpoints VALIDATE CONSTRAINT fk_document_checkpoints_document`,

		`DO $$ BEGIN ALTER TABLE threads ADD CONSTRAINT fk_threads_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE threads VALIDATE CONSTRAINT fk_threads_document`,

		`DO $$ BEGIN ALTER TABLE presences ADD CONSTRAINT fk_presences_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE presences VALIDATE CONSTRAINT fk_presences_document`,

		`DO $$ BEGIN ALTER TABLE agent_document_views ADD CONSTRAINT fk_agent_document_views_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_document_views VALIDATE CONSTRAINT fk_agent_document_views_document`,

		// SET NULL FKs for optional document refs use composite keys with column-list SET NULL
		// (Postgres 15+) to null only the ref column, not workspace_id.
		`DO $$ BEGIN ALTER TABLE workspace_members ADD CONSTRAINT fk_workspace_members_last_doc FOREIGN KEY (workspace_id, last_accessed_document_id) REFERENCES documents(workspace_id, id) ON DELETE SET NULL (last_accessed_document_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE workspace_members VALIDATE CONSTRAINT fk_workspace_members_last_doc`,

		`DO $$ BEGIN ALTER TABLE activities ADD CONSTRAINT fk_activities_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE SET NULL (document_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE activities VALIDATE CONSTRAINT fk_activities_document`,

		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_document FOREIGN KEY (workspace_id, document_id) REFERENCES documents(workspace_id, id) ON DELETE SET NULL (document_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_document`,

		// ── Threads/messages (composite same-workspace enforcement) ──

		`DO $$ BEGIN ALTER TABLE thread_messages ADD CONSTRAINT fk_thread_messages_thread FOREIGN KEY (workspace_id, thread_id) REFERENCES threads(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE thread_messages VALIDATE CONSTRAINT fk_thread_messages_thread`,

		`DO $$ BEGIN ALTER TABLE thread_participants ADD CONSTRAINT fk_thread_participants_thread FOREIGN KEY (workspace_id, thread_id) REFERENCES threads(workspace_id, id) ON DELETE CASCADE NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE thread_participants VALIDATE CONSTRAINT fk_thread_participants_thread`,

		// SET NULL FKs for optional thread/message refs use composite keys with column-list SET NULL.
		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_thread FOREIGN KEY (workspace_id, thread_id) REFERENCES threads(workspace_id, id) ON DELETE SET NULL (thread_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_thread`,

		`DO $$ BEGIN ALTER TABLE agent_events ADD CONSTRAINT fk_agent_events_thread_message FOREIGN KEY (workspace_id, thread_message_id) REFERENCES thread_messages(workspace_id, id) ON DELETE SET NULL (thread_message_id) NOT VALID; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
		`ALTER TABLE agent_events VALIDATE CONSTRAINT fk_agent_events_thread_message`,

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
	return nil
}

func (s *Store) loadNormalizedPostgresLocked() error {
	s.state.ContentDocuments = map[string]*Document{}
	s.state.Users = map[string]*User{}
	s.state.Daemons = map[string]*Daemon{}
	s.state.Agents = map[string]*Agent{}
	s.state.AgentRuns = map[string]*AgentRun{}
	s.state.Presences = map[string]*Presence{}
	s.state.Activities = []*ActivityEvent{}
	s.state.Threads = map[string]*Thread{}
	s.state.AgentEvents = map[string]*AgentEvent{}
	s.state.AgentDocumentViews = map[string]*AgentDocumentView{}
	s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}

	if err := s.ensurePostgresCheckpointsLocked(); err != nil {
		return fmt.Errorf("ensure checkpoints: %w", err)
	}
	if err := s.loadDocumentsPostgresLocked(); err != nil {
		return fmt.Errorf("load documents: %w", err)
	}
	if err := s.loadUsersPostgresLocked(); err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	if err := s.loadDaemonsPostgresLocked(); err != nil {
		return fmt.Errorf("load daemons: %w", err)
	}
	if err := s.loadAgentsPostgresLocked(); err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	if err := s.loadAgentRunsPostgresLocked(); err != nil {
		return fmt.Errorf("load agent runs: %w", err)
	}
	if err := s.loadPresencesPostgresLocked(); err != nil {
		return fmt.Errorf("load presences: %w", err)
	}
	if err := s.loadActivitiesPostgresLocked(); err != nil {
		return fmt.Errorf("load activities: %w", err)
	}
	if err := s.loadThreadsPostgresLocked(); err != nil {
		return fmt.Errorf("load threads: %w", err)
	}
	if err := s.loadAgentEventsPostgresLocked(); err != nil {
		return fmt.Errorf("load agent events: %w", err)
	}
	if err := s.loadAgentDocumentViewsPostgresLocked(); err != nil {
		return fmt.Errorf("load agent document views: %w", err)
	}
	return nil
}

func (s *Store) loadWorkspaceMetadataPostgresLocked() error {
	var name string
	var rootDocumentID string
	err := s.db.QueryRow(
		`SELECT name, root_document_id::text FROM workspaces WHERE id::text = $1`,
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
	rootDocumentID = strings.TrimSpace(rootDocumentID)
	if rootDocumentID == "" {
		return errors.New("workspace root document id is required")
	}
	s.state.RootDocumentID = rootDocumentID
	return nil
}

func (s *Store) persistPostgresLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = s.persistDocumentsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceUsersPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceAgentsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceAgentRunsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.upsertPresencesPostgresLocked(tx); err != nil {
		return err
	}
	activityInserts, err := s.insertActivitiesPostgresLocked(tx)
	if err != nil {
		return err
	}
	if err = s.upsertThreadsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.upsertAgentEventsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.upsertAgentDocumentViewsPostgresLocked(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, insert := range activityInserts {
		if insert.activity != nil {
			insert.activity.ID = insert.id
		}
	}
	s.dirtyAgentEvents = false
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

	if err = s.persistDocumentsPostgresLocked(tx); err != nil {
		return err
	}
	if s.dirtyAgentEvents {
		if err = s.upsertAgentEventsPostgresLocked(tx); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.dirtyAgentEvents = false
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

func (s *Store) persistDocumentsPostgresLocked(tx *sql.Tx) error {
	for documentID := range s.dirtyDocuments {
		document := s.state.ContentDocuments[documentID]
		if document == nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed, create_client_operation_id, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (id)
			 DO UPDATE SET path = EXCLUDED.path,
			               title = EXCLUDED.title,
			               hidden = EXCLUDED.hidden,
			               client_id_seed = EXCLUDED.client_id_seed,
			               create_client_operation_id = EXCLUDED.create_client_operation_id,
			               updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
			document.ID,
			"",
			"",
			document.Hidden,
			int64(document.ClientIDSeed),
			document.CreateClientOperationID,
			document.UpdatedAt,
		); err != nil {
			return err
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
			return err
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
			s.enqueueDocumentInboxEventsLocked(document, latestMetaByDocument[documentID])
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
				  WHERE workspace_id::text = $4 AND document_id::text = $5`,
				document.StateVector,
				document.UpdateID,
				document.UpdatedAt,
				s.state.WorkspaceID,
				document.ID,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
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
					return err
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
			return err
		}
		if err := s.maybeInsertDocumentCheckpointPostgresLocked(tx, document); err != nil {
			return err
		}
	}
	s.dirtyDocuments = map[string]struct{}{}
	s.pendingDocumentEvents = []documentUpdateRecord{}
	return nil
}

func (s *Store) maybeInsertDocumentCheckpointPostgresLocked(tx *sql.Tx, document *Document) error {
	if document == nil || document.UpdateID <= 0 {
		return nil
	}
	var lastCheckpointID int64
	err := tx.QueryRow(
		`SELECT COALESCE(MAX(update_id), 0)
		   FROM document_checkpoints
		  WHERE workspace_id::text = $1 AND document_id::text = $2`,
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

func (s *Store) replaceUsersPostgresLocked(tx *sql.Tx) error {
	// Upsert current users.
	for _, user := range s.state.Users {
		if _, err := tx.Exec(
			`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (id)
			 DO UPDATE SET handle = EXCLUDED.handle,
			               name = EXCLUDED.name,
			               role = EXCLUDED.role,
			               kind = EXCLUDED.kind,
			               status = EXCLUDED.status,
			               updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
			user.ID,
			user.Handle,
			user.Name,
			user.Role,
			user.Kind,
			user.Status,
			user.CreatedAt,
			user.UpdatedAt,
		); err != nil {
			return err
		}
	}
	// Remove users that are no longer in the in-memory state.
	ids := make([]string, 0, len(s.state.Users))
	for id := range s.state.Users {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		_, err := tx.Exec(`DELETE FROM users WHERE workspace_id::text = $1`, s.state.WorkspaceID)
		return err
	}
	// Build a VALUES list for the keep set.
	keepQuery := `DELETE FROM users WHERE workspace_id::text = $1 AND id NOT IN (`
	args := []any{s.state.WorkspaceID}
	for i, id := range ids {
		if i > 0 {
			keepQuery += ","
		}
		keepQuery += fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	keepQuery += ")"
	_, err := tx.Exec(keepQuery, args...)
	return err
}

func (s *Store) replaceAgentsPostgresLocked(tx *sql.Tx) error {
	// Upsert current agents.
	for _, agent := range s.state.Agents {
		if err := upsertAgentPostgresTx(tx, s.state.WorkspaceID, agent); err != nil {
			return err
		}
	}
	// Remove agents no longer in state.
	ids := make([]string, 0, len(s.state.Agents))
	for id := range s.state.Agents {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		_, err := tx.Exec(`DELETE FROM agents WHERE workspace_id::text = $1`, s.state.WorkspaceID)
		return err
	}
	keepQuery := `DELETE FROM agents WHERE workspace_id::text = $1 AND id NOT IN (`
	args := []any{s.state.WorkspaceID}
	for i, id := range ids {
		if i > 0 {
			keepQuery += ","
		}
		keepQuery += fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	keepQuery += ")"
	_, err := tx.Exec(keepQuery, args...)
	return err
}

func upsertAgentPostgres(db *sql.DB, workspaceID string, agent *Agent) error {
	if agent == nil {
		return nil
	}
	_, err := db.Exec(
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

func (s *Store) replaceAgentRunsPostgresLocked(tx *sql.Tx) error {
	for _, run := range s.state.AgentRuns {
		logTail, err := json.Marshal(run.LogTail)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
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
			)
			ON CONFLICT (id)
			DO UPDATE SET status = EXCLUDED.status,
			              desired_status = EXCLUDED.desired_status,
			              process_id = EXCLUDED.process_id,
			              launch_time = EXCLUDED.launch_time,
			              last_heartbeat_at = EXCLUDED.last_heartbeat_at,
			              completed_at = EXCLUDED.completed_at,
			              exit_code = EXCLUDED.exit_code,
			              last_message = EXCLUDED.last_message,
			              log_tail = EXCLUDED.log_tail,
			              error = EXCLUDED.error,
			              session_id = EXCLUDED.session_id,
			              updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
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
		); err != nil {
			return err
		}
	}
	// Remove agent runs no longer in state.
	ids := make([]string, 0, len(s.state.AgentRuns))
	for id := range s.state.AgentRuns {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		_, err := tx.Exec(`DELETE FROM agent_runs WHERE workspace_id::text = $1`, s.state.WorkspaceID)
		return err
	}
	keepQuery := `DELETE FROM agent_runs WHERE workspace_id::text = $1 AND id NOT IN (`
	args := []any{s.state.WorkspaceID}
	for i, id := range ids {
		if i > 0 {
			keepQuery += ","
		}
		keepQuery += fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	keepQuery += ")"
	_, err := tx.Exec(keepQuery, args...)
	return err
}

func (s *Store) upsertPresencesPostgresLocked(tx *sql.Tx) error {
	for _, presence := range s.state.Presences {
		if presence == nil {
			continue
		}
		start, end := selectionBounds(presence.Selection)
		if _, err := tx.Exec(
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
			s.state.WorkspaceID,
			presence.ActorID,
			presence.ActorType,
			presence.DocumentID,
			presence.FilePath,
			presence.Mode,
			start,
			end,
			presence.Activity,
			presence.UpdatedAt,
		); err != nil {
			return err
		}
	}
	return nil
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
		presence.DocumentID,
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

type persistedActivityInsert struct {
	activity *ActivityEvent
	id       int64
}

func (s *Store) insertActivitiesPostgresLocked(tx *sql.Tx) ([]persistedActivityInsert, error) {
	inserts := []persistedActivityInsert{}
	for _, activity := range s.state.Activities {
		if activity == nil || activity.ID != 0 {
			continue
		}
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO activities (
				workspace_id, type, document_id, actor_id, actor_type, summary, occurred_at,
				provenance_actor_id, provenance_actor_type, provenance_execution_id, provenance_tool,
					provenance_trigger, provenance_autonomous, provenance_confidence, provenance_requested_by,
					provenance_source, provenance_intended_scope, provenance_read_set_summary,
					comment_id, presence_ref
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7,
					$8, $9, $10, $11,
					$12, $13, $14, $15,
					$16, $17, $18,
					$19, $20
				)
				RETURNING id`,
			s.state.WorkspaceID,
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
			"",
			activity.PresenceRef,
		).Scan(&id); err != nil {
			return nil, err
		}
		inserts = append(inserts, persistedActivityInsert{activity: activity, id: id})
	}
	return inserts, nil
}

func (s *Store) upsertThreadsPostgresLocked(tx *sql.Tx) error {
	for _, thread := range s.state.Threads {
		if _, err := tx.Exec(
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
				ON CONFLICT (id)
				DO UPDATE SET document_id = EXCLUDED.document_id,
				              client_operation_id = EXCLUDED.client_operation_id,
				              title = EXCLUDED.title,
				              status = EXCLUDED.status,
				              anchor_relative_start = EXCLUDED.anchor_relative_start,
				              anchor_relative_end = EXCLUDED.anchor_relative_end,
				              anchor_kind = EXCLUDED.anchor_kind,
				              anchor_excerpt = EXCLUDED.anchor_excerpt,
				              created_by_id = EXCLUDED.created_by_id,
				              created_by_type = EXCLUDED.created_by_type,
				              created_by_handle = EXCLUDED.created_by_handle,
				              created_by_name = EXCLUDED.created_by_name,
				              created_at = EXCLUDED.created_at,
				              updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
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
		); err != nil {
			return err
		}
		for _, participantID := range thread.ParticipantIDs {
			if _, err := tx.Exec(
				`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
					 VALUES ($1, $2, $3)
					 ON CONFLICT (workspace_id, thread_id, participant_id) DO NOTHING`,
				s.state.WorkspaceID,
				thread.ID,
				participantID,
			); err != nil {
				return err
			}
		}
		for _, message := range thread.Messages {
			if _, err := tx.Exec(
				`INSERT INTO thread_messages (
						workspace_id, id, thread_id, author_id, author_type,
						author_handle, author_name, body, kind, created_at
					) VALUES (
						$1, $2, $3, $4, $5,
						$6, $7, $8, $9, $10
					)
					ON CONFLICT (id)
					DO UPDATE SET thread_id = EXCLUDED.thread_id,
					              author_id = EXCLUDED.author_id,
					              author_type = EXCLUDED.author_type,
					              author_handle = EXCLUDED.author_handle,
					              author_name = EXCLUDED.author_name,
					              body = EXCLUDED.body,
					              kind = EXCLUDED.kind,
					              created_at = EXCLUDED.created_at`,
				s.state.WorkspaceID,
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
				return err
			}
		}
	}
	return nil
}

func (s *Store) upsertAgentEventsPostgresLocked(tx *sql.Tx) error {
	for _, event := range s.state.AgentEvents {
		if event == nil {
			continue
		}
		if _, err := tx.Exec(
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
			ON CONFLICT (id)
			DO UPDATE SET
				agent_id = EXCLUDED.agent_id,
				agent_handle = EXCLUDED.agent_handle,
				type = EXCLUDED.type,
				box = EXCLUDED.box,
				status = EXCLUDED.status,
				document_id = EXCLUDED.document_id,
				thread_id = EXCLUDED.thread_id,
				thread_message_id = EXCLUDED.thread_message_id,
				from_update_id = EXCLUDED.from_update_id,
				to_update_id = EXCLUDED.to_update_id,
				summary = EXCLUDED.summary,
				prompt = EXCLUDED.prompt,
				dedup_key = EXCLUDED.dedup_key,
				claimed_by = EXCLUDED.claimed_by,
				run_id = EXCLUDED.run_id,
				last_error = EXCLUDED.last_error,
				attempt_count = EXCLUDED.attempt_count,
				available_at = EXCLUDED.available_at,
				claimed_at = EXCLUDED.claimed_at,
				completed_at = EXCLUDED.completed_at,
				updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
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
		); err != nil {
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

func (s *Store) upsertAgentDocumentViewsPostgresLocked(tx *sql.Tx) error {
	for _, view := range s.state.AgentDocumentViews {
		if view == nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (workspace_id, agent_id, document_id)
			 DO UPDATE SET update_id = EXCLUDED.update_id,
			               state_vector = EXCLUDED.state_vector,
			               viewed_at = EXCLUDED.viewed_at`,
			s.state.WorkspaceID,
			view.AgentID,
			view.DocumentID,
			view.UpdateID,
			view.StateVector,
			view.ViewedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func upsertAgentDocumentViewPostgres(db *sql.DB, workspaceID string, view *AgentDocumentView) error {
	if view == nil {
		return nil
	}
	_, err := db.Exec(
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

func completeDocumentInboxEventsPostgres(db *sql.DB, workspaceID string, agentID string, documentID string, updateID int64, completedAt time.Time) error {
	_, err := db.Exec(
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

func (s *Store) loadAgentDocumentViewsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT agent_id::text, document_id::text, update_id, state_vector, viewed_at
		   FROM agent_document_views
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		view := &AgentDocumentView{}
		if err := rows.Scan(&view.AgentID, &view.DocumentID, &view.UpdateID, &view.StateVector, &view.ViewedAt); err != nil {
			return err
		}
		s.state.AgentDocumentViews[agentDocumentViewKey(view.AgentID, view.DocumentID)] = view
	}
	return rows.Err()
}

func (s *Store) documentContentAtUpdatePostgresLocked(document *Document, updateID int64) (string, error) {
	return documentContentAtUpdatePostgres(s.db, s.state.WorkspaceID, document, updateID)
}

func documentContentAtUpdatePostgres(db *sql.DB, workspaceID string, document *Document, updateID int64) (string, error) {
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(document.ClientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	err := db.QueryRow(
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id::text = $1 AND document_id::text = $2 AND update_id <= $3
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
		  WHERE workspace_id::text = $1
		    AND document_id::text = $2
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
		     ON h.workspace_id::text = d.workspace_id::text AND h.document_id::text = d.id::text
		   LEFT JOIN LATERAL (
		       SELECT update_id
		         FROM document_checkpoints c
		        WHERE c.workspace_id::text = d.workspace_id::text
		          AND c.document_id::text = d.id::text
		          AND c.update_id <= h.update_id
		        ORDER BY c.update_id DESC
		        LIMIT 1
		   ) checkpoint ON TRUE
		  WHERE d.workspace_id::text = $1
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
		  WHERE workspace_id::text = $1 AND document_id::text = $2 AND update_id <= $3
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
		  WHERE workspace_id::text = $1
		    AND document_id::text = $2
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

func (s *Store) loadUsersPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, handle, name, role, kind, status, created_at, updated_at
		   FROM users
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

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
			return err
		}
		s.state.Users[user.ID] = user
	}
	return rows.Err()
}

func (s *Store) loadDaemonsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, workspace_id::text, name, status, daemon_version, os, arch, runtime_detections::text, last_seen_at, created_at, deleted_at
		   FROM daemons
		  WHERE workspace_id::text = $1
		    AND status <> 'deleted'`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		daemon := &Daemon{}
		var lastSeen sql.NullTime
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&daemon.ID,
			&daemon.WorkspaceID,
			&daemon.Name,
			&daemon.Status,
			&daemon.Version,
			&daemon.OS,
			&daemon.Arch,
			runtimeDetectionsScanner(&daemon.Runtimes),
			&lastSeen,
			&daemon.CreatedAt,
			&deletedAt,
		); err != nil {
			return err
		}
		if lastSeen.Valid {
			daemon.LastSeenAt = lastSeen.Time
		}
		if deletedAt.Valid {
			daemon.DeletedAt = deletedAt.Time
		}
		s.state.Daemons[daemon.ID] = daemon
	}
	return rows.Err()
}

func (s *Store) loadAgentsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, COALESCE(daemon_id::text, ''), handle, name, role, kind, system_prompt, workspace_root,
		        current_turn_id, session_id, status,
		        current_task, current_activity, COALESCE(current_run_id::text, ''), last_heartbeat_at,
		        last_run_completed, updated_at
		   FROM agents
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		agent := &Agent{}
		var lastHeartbeat sql.NullTime
		var lastRunCompleted sql.NullTime
		if err := rows.Scan(
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
			return err
		}
		if lastHeartbeat.Valid {
			agent.LastHeartbeatAt = lastHeartbeat.Time
		}
		if lastRunCompleted.Valid {
			agent.LastRunCompleted = lastRunCompleted.Time
		}
		s.state.Agents[agent.ID] = agent
	}
	return rows.Err()
}

func (s *Store) loadAgentRunsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, agent_id::text, agent_handle, agent_name, agent_kind, system_prompt, session_id,
		        workspace_root, working_dir, prompt, status, desired_status, process_id,
		        launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
		        log_tail, error, assigned_task_ref, updated_at
		   FROM agent_runs
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return err
		}
		run.WorkspaceID = s.state.WorkspaceID
		s.state.AgentRuns[run.ID] = run
	}
	return rows.Err()
}

func (s *Store) loadPresencesPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT actor_id::text, actor_type, document_id::text, file_path, mode, selection_start, selection_end, activity, updated_at
		   FROM presences
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		presence := &Presence{}
		var start sql.NullInt64
		var end sql.NullInt64
		if err := rows.Scan(
			&presence.ActorID,
			&presence.ActorType,
			&presence.DocumentID,
			&presence.FilePath,
			&presence.Mode,
			&start,
			&end,
			&presence.Activity,
			&presence.UpdatedAt,
		); err != nil {
			return err
		}
		presence.Selection = selectionFromNulls(start, end)
		s.state.Presences[presence.ActorID] = presence
	}
	return rows.Err()
}

func (s *Store) loadActivitiesPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id, type, COALESCE(document_id::text, ''), COALESCE(actor_id::text, ''), actor_type, summary, occurred_at,
		        COALESCE(provenance_actor_id::text, ''), provenance_actor_type, provenance_execution_id,
		        provenance_tool, provenance_trigger, provenance_autonomous,
		        provenance_confidence, provenance_requested_by, provenance_source,
		        provenance_intended_scope, provenance_read_set_summary,
		        presence_ref
		   FROM activities
		  WHERE workspace_id::text = $1
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT 100`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

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
			return err
		}
		s.state.Activities = append(s.state.Activities, activity)
	}
	return rows.Err()
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
		     ON h.workspace_id::text = d.workspace_id::text AND h.document_id::text = d.id::text
		   LEFT JOIN LATERAL (
		       SELECT state_vector
		         FROM document_checkpoints c
		        WHERE c.workspace_id::text = d.workspace_id::text
		          AND c.document_id::text = d.id::text
		          AND c.update_id <= h.update_id
		        ORDER BY c.update_id DESC
		        LIMIT 1
		   ) checkpoint ON TRUE
		  WHERE d.workspace_id::text = $1
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
		  WHERE workspace_id::text = $1 AND document_id::text = $2 AND update_id <= $3
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
		  WHERE workspace_id::text = $1
		    AND document_id::text = $2
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

func (s *Store) loadThreadsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, document_id::text, client_operation_id, title, status, anchor_relative_start, anchor_relative_end,
		        anchor_kind, anchor_excerpt, COALESCE(created_by_id::text, ''), created_by_type,
		        created_by_handle, created_by_name, created_at, updated_at
		   FROM threads
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return err
		}
		s.state.Threads[thread.ID] = thread
	}
	if err := rows.Err(); err != nil {
		return err
	}

	participants, err := s.db.Query(
		`SELECT thread_id::text, participant_id::text
		   FROM thread_participants
		  WHERE workspace_id::text = $1
		  ORDER BY thread_id, participant_id`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer participants.Close()
	for participants.Next() {
		var threadID, participantID string
		if err := participants.Scan(&threadID, &participantID); err != nil {
			return err
		}
		thread := s.state.Threads[threadID]
		if thread == nil {
			continue
		}
		thread.ParticipantIDs = append(thread.ParticipantIDs, participantID)
	}
	if err := participants.Err(); err != nil {
		return err
	}

	messages, err := s.db.Query(
		`SELECT id::text, thread_id::text, COALESCE(author_id::text, ''), author_type, author_handle, author_name, body, kind, created_at
		   FROM thread_messages
		  WHERE workspace_id::text = $1
		  ORDER BY created_at ASC, id ASC`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer messages.Close()
	for messages.Next() {
		message, threadID, err := scanThreadMessage(messages)
		if err != nil {
			return err
		}
		thread := s.state.Threads[threadID]
		if thread == nil {
			continue
		}
		thread.Messages = append(thread.Messages, message)
	}
	return messages.Err()
}

func (s *Store) loadAgentEventsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id::text, agent_id::text, agent_handle, type, box, status, COALESCE(document_id::text, ''), COALESCE(thread_id::text, ''),
		        COALESCE(thread_message_id::text, ''), from_update_id, to_update_id,
		        summary, prompt, dedup_key, COALESCE(claimed_by::text, ''), COALESCE(run_id::text, ''), last_error, attempt_count,
		        available_at, claimed_at, completed_at, created_at, updated_at
		   FROM agent_events
		  WHERE workspace_id::text = $1`,
		s.state.WorkspaceID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		event, err := scanAgentEvent(rows)
		if err != nil {
			return err
		}
		s.state.AgentEvents[event.ID] = event
	}
	return rows.Err()
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
	thread := &Thread{Messages: []*ThreadMessage{}, ParticipantIDs: []string{}}
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
		return nil
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
