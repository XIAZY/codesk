package notty

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const postgresCheckpointInterval = 100

var ErrLegacyDocumentsNeedMigration = errors.New("legacy document tables require migration")

var legacyDocumentTables = []string{
	"document_checkpoints",
	"document_updates",
	"document_heads",
	"documents",
	"document_mentions",
}

func initPostgresSchemaTables(db *sql.DB) error {
	if err := guardLegacyDocumentTablesEmpty(context.Background(), db); err != nil {
		return err
	}
	statements := []string{
		`
		CREATE TABLE IF NOT EXISTS workspaces (
			id TEXT PRIMARY KEY,
			slug TEXT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			root_stream_id TEXT,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS root_stream_id TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_root_stream_id ON workspaces (root_stream_id) WHERE root_stream_id IS NOT NULL`,
		`
		CREATE TABLE IF NOT EXISTS crdt_stream_heads (
			workspace_id TEXT NOT NULL,
			stream_id TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'unknown',
			state_vector BYTEA NOT NULL DEFAULT '\x',
			update_id BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, stream_id)
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS crdt_stream_updates (
			id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			stream_id TEXT NOT NULL,
			update BYTEA NOT NULL,
			update_sha256 TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_crdt_stream_updates_stream_id ON crdt_stream_updates (workspace_id, stream_id, id ASC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_crdt_stream_updates_dedupe ON crdt_stream_updates (workspace_id, stream_id, update_sha256)`,
		`
		CREATE TABLE IF NOT EXISTS crdt_stream_checkpoints (
			id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			stream_id TEXT NOT NULL,
			update_id BIGINT NOT NULL,
			crdt_state BYTEA NOT NULL,
			state_vector BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (workspace_id, stream_id, update_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_crdt_stream_checkpoints_stream_update ON crdt_stream_checkpoints (workspace_id, stream_id, update_id DESC)`,
		`
		CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			password_updated_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS workspace_members (
			workspace_id TEXT NOT NULL,
			account_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			membership_role TEXT NOT NULL DEFAULT 'member',
			status TEXT NOT NULL DEFAULT 'active',
			invited_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			accepted_at TIMESTAMPTZ,
			PRIMARY KEY (workspace_id, account_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_workspace_members_account ON workspace_members (account_id, status, workspace_id)`,
		`
		CREATE TABLE IF NOT EXISTS daemons (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			name TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'active',
			last_seen_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_daemons_workspace ON daemons (workspace_id, status)`,
		`DROP TABLE IF EXISTS document_checkpoints`,
		`DROP TABLE IF EXISTS document_updates`,
		`DROP TABLE IF EXISTS document_heads`,
		`DROP TABLE IF EXISTS documents`,
		`DROP TABLE IF EXISTS document_mentions`,
		`
		CREATE TABLE IF NOT EXISTS users (
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
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
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			daemon_id TEXT NOT NULL DEFAULT '',
			handle TEXT NOT NULL,
			name TEXT NOT NULL,
			role TEXT NOT NULL,
			kind TEXT NOT NULL,
			system_prompt TEXT NOT NULL,
			workspace_root TEXT NOT NULL,
			codex_thread_id TEXT NOT NULL DEFAULT '',
			current_turn_id TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			current_task TEXT NOT NULL,
			current_activity TEXT NOT NULL,
			current_run_id TEXT NOT NULL,
			last_heartbeat_at TIMESTAMPTZ,
			last_run_completed TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_workspace_handle ON agents (workspace_id, handle)`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS daemon_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS codex_thread_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN IF NOT EXISTS current_turn_id TEXT NOT NULL DEFAULT ''`,
		`
		CREATE TABLE IF NOT EXISTS agent_runs (
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
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
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			document_id TEXT NOT NULL,
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			anchor_relative_start TEXT NOT NULL DEFAULT '',
			anchor_relative_end TEXT NOT NULL DEFAULT '',
			anchor_kind TEXT NOT NULL DEFAULT '',
			anchor_excerpt TEXT NOT NULL DEFAULT '',
			created_by_id TEXT NOT NULL,
			created_by_type TEXT NOT NULL,
			created_by_handle TEXT NOT NULL,
			created_by_name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_threads_workspace_document_updated ON threads (workspace_id, document_id, updated_at DESC)`,
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
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			thread_id TEXT NOT NULL,
			author_id TEXT NOT NULL,
			author_type TEXT NOT NULL,
			author_handle TEXT NOT NULL,
			author_name TEXT NOT NULL,
			body TEXT NOT NULL,
			kind TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_thread_messages_workspace_thread_created ON thread_messages (workspace_id, thread_id, created_at ASC)`,
		`
		CREATE TABLE IF NOT EXISTS thread_participants (
			workspace_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			participant_id TEXT NOT NULL,
			PRIMARY KEY (workspace_id, thread_id, participant_id)
		)
		`,
		`
		CREATE TABLE IF NOT EXISTS presences (
			workspace_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			document_id TEXT NOT NULL,
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
			workspace_id TEXT NOT NULL,
			type TEXT NOT NULL,
			document_id TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			summary TEXT NOT NULL,
			occurred_at TIMESTAMPTZ NOT NULL,
			provenance_actor_id TEXT NOT NULL,
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
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			agent_handle TEXT NOT NULL,
			type TEXT NOT NULL,
			box TEXT NOT NULL DEFAULT 'for_me',
			status TEXT NOT NULL,
			document_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			thread_message_id TEXT NOT NULL,
			from_update_id BIGINT NOT NULL DEFAULT 0,
			to_update_id BIGINT NOT NULL DEFAULT 0,
			summary TEXT NOT NULL,
			prompt TEXT NOT NULL,
			dedup_key TEXT NOT NULL,
			claimed_by TEXT NOT NULL,
			run_id TEXT NOT NULL,
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
			workspace_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			document_id TEXT NOT NULL,
			update_id BIGINT NOT NULL,
			state_vector TEXT NOT NULL,
			viewed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (workspace_id, agent_id, document_id)
		)
		`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func guardLegacyDocumentTablesEmpty(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database is required")
	}
	for _, table := range legacyDocumentTables {
		exists, err := legacyDocumentTableExists(ctx, db, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		var count int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w: %s contains %d rows", ErrLegacyDocumentsNeedMigration, table, count)
		}
	}
	return nil
}

func legacyDocumentTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM information_schema.tables
			 WHERE table_schema = current_schema()
			   AND table_name = $1
		)`, table).Scan(&exists)
	return exists, err
}

func (s *Store) loadNormalizedPostgresLocked() error {
	s.state.Documents = map[string]*Document{}
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

	if err := s.loadUsersPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadDaemonsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadAgentsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadAgentRunsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadPresencesPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadActivitiesPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadThreadsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadAgentEventsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadAgentDocumentViewsPostgresLocked(); err != nil {
		return err
	}
	if err := s.refreshStreamDocumentCacheLocked(); err != nil {
		return err
	}
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

	if err = s.replaceUsersPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceAgentsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceAgentRunsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replacePresencesPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceActivitiesPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceThreadsPostgresLocked(tx); err != nil {
		return err
	}
	if err = s.replaceAgentEventsPostgresLocked(tx); err != nil {
		return err
	}
	s.dirtyAgentEvents = false
	if err = s.replaceAgentDocumentViewsPostgresLocked(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) replaceUsersPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM users WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, user := range s.state.Users {
		if _, err := tx.Exec(
			`INSERT INTO users (workspace_id, id, handle, name, role, kind, status, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
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
	return nil
}

func (s *Store) replaceAgentsPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM agents WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, agent := range s.state.Agents {
		if err := insertAgentPostgres(tx, s.state.WorkspaceID, agent); err != nil {
			return err
		}
	}
	return nil
}

func upsertAgentPostgres(db *sql.DB, workspaceID string, agent *Agent) error {
	if agent == nil {
		return nil
	}
	_, err := db.Exec(
		`INSERT INTO agents (
			workspace_id, id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
			codex_thread_id, current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15,
			$16, $17, $18, $19
		)
		ON CONFLICT (id)
		DO UPDATE SET daemon_id = EXCLUDED.daemon_id,
		              handle = EXCLUDED.handle,
		              name = EXCLUDED.name,
		              role = EXCLUDED.role,
		              kind = EXCLUDED.kind,
		              system_prompt = EXCLUDED.system_prompt,
		              workspace_root = EXCLUDED.workspace_root,
		              codex_thread_id = EXCLUDED.codex_thread_id,
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
		agent.DaemonID,
		agent.Handle,
		agent.Name,
		agent.Role,
		agent.Kind,
		agent.SystemPrompt,
		agent.WorkspaceRoot,
		agent.CodexThreadID,
		agent.CurrentTurnID,
		agent.SessionID,
		agent.Status,
		agent.CurrentTask,
		agent.CurrentActivity,
		agent.CurrentRunID,
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
			codex_thread_id, current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15,
			$16, $17, $18, $19
		)`,
		workspaceID,
		agent.ID,
		agent.DaemonID,
		agent.Handle,
		agent.Name,
		agent.Role,
		agent.Kind,
		agent.SystemPrompt,
		agent.WorkspaceRoot,
		agent.CodexThreadID,
		agent.CurrentTurnID,
		agent.SessionID,
		agent.Status,
		agent.CurrentTask,
		agent.CurrentActivity,
		agent.CurrentRunID,
		nullTime(agent.LastHeartbeatAt),
		nullTime(agent.LastRunCompleted),
		agent.UpdatedAt,
	)
	return err
}

func (s *Store) replaceAgentRunsPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM agent_runs WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
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
			)`,
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
	return nil
}

func (s *Store) replacePresencesPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM presences WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, presence := range s.state.Presences {
		start, end := selectionBounds(presence.Selection)
		if _, err := tx.Exec(
			`INSERT INTO presences (
				workspace_id, actor_id, actor_type, document_id, file_path, mode,
				selection_start, selection_end, activity, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, $10
			)`,
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

func (s *Store) replaceActivitiesPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM activities WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, activity := range s.state.Activities {
		if _, err := tx.Exec(
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
				)`,
			s.state.WorkspaceID,
			activity.Type,
			activity.DocumentID,
			activity.ActorID,
			activity.ActorType,
			activity.Summary,
			activity.OccurredAt,
			activity.Provenance.ActorID,
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
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) replaceThreadsPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM thread_messages WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM thread_participants WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM threads WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, thread := range s.state.Threads {
		if _, err := tx.Exec(
			`INSERT INTO threads (
				workspace_id, id, document_id, title, status,
				anchor_relative_start, anchor_relative_end, anchor_kind, anchor_excerpt,
				created_by_id, created_by_type, created_by_handle, created_by_name,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9,
				$10, $11, $12, $13,
				$14, $15
			)`,
			s.state.WorkspaceID,
			thread.ID,
			thread.DocumentID,
			thread.Title,
			thread.Status,
			thread.Anchor.RelativeStart,
			thread.Anchor.RelativeEnd,
			thread.Anchor.Kind,
			thread.Anchor.Excerpt,
			thread.CreatedByID,
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
				`INSERT INTO thread_participants (workspace_id, thread_id, participant_id) VALUES ($1, $2, $3)`,
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
				)`,
				s.state.WorkspaceID,
				message.ID,
				thread.ID,
				message.AuthorID,
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

func (s *Store) replaceAgentEventsPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM agent_events WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, event := range s.state.AgentEvents {
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
			)`,
			s.state.WorkspaceID,
			event.ID,
			event.AgentID,
			event.AgentHandle,
			event.Type,
			normalizeInboxBox(event.Box),
			event.Status,
			event.DocumentID,
			event.ThreadID,
			event.ThreadMessageID,
			event.FromUpdateID,
			event.ToUpdateID,
			event.Summary,
			event.Prompt,
			event.DedupKey,
			event.ClaimedBy,
			event.RunID,
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
			RETURNING agent_events.id, agent_events.agent_id, agent_events.agent_handle, agent_events.type,
			          agent_events.box, agent_events.status, agent_events.document_id, agent_events.thread_id,
			          agent_events.thread_message_id, agent_events.from_update_id, agent_events.to_update_id, agent_events.summary,
			          agent_events.prompt, agent_events.dedup_key,
			          agent_events.claimed_by, agent_events.run_id, agent_events.last_error,
			          agent_events.attempt_count, agent_events.available_at, agent_events.claimed_at,
			          agent_events.completed_at, agent_events.created_at, agent_events.updated_at`,
		workspaceID,
		agentID,
		now,
		now.Add(-30*time.Second),
		agentHandle,
		claimedBy,
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
		        thread_id = CASE WHEN $2 <> '' THEN $2 ELSE thread_id END,
		        run_id = CASE WHEN $3 <> '' THEN $3 ELSE run_id END,
		        last_error = CASE WHEN $4 <> '' THEN $4 ELSE last_error END,
		        completed_at = CASE WHEN $1 = 'completed' THEN $5 ELSE completed_at END,
		        available_at = CASE WHEN $1 = 'pending' AND available_at < $5 THEN $6 ELSE available_at END,
		        updated_at = $5
		  WHERE workspace_id = $7
		    AND id = $8
		RETURNING id, agent_id, agent_handle, type, box, status, document_id, thread_id,
		          thread_message_id, from_update_id, to_update_id,
		          summary, prompt, dedup_key, claimed_by, run_id, last_error, attempt_count,
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

func (s *Store) replaceAgentDocumentViewsPostgresLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM agent_document_views WHERE workspace_id = $1`, s.state.WorkspaceID); err != nil {
		return err
	}
	for _, view := range s.state.AgentDocumentViews {
		if view == nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector, viewed_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
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
		`SELECT agent_id, document_id, update_id, state_vector, viewed_at
		   FROM agent_document_views
		  WHERE workspace_id = $1`,
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
	doc, _, err := restoreStreamDocAtUpdate(db, workspaceID, document.ID, updateID)
	if err != nil {
		return "", err
	}
	return doc.GetText("content").ToString(), nil
}

func (s *Store) loadUsersPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id, handle, name, role, kind, status, created_at, updated_at
		   FROM users
		  WHERE workspace_id = $1`,
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
		`SELECT id, workspace_id, name, status, last_seen_at, created_at, deleted_at
		   FROM daemons
		  WHERE workspace_id = $1
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
		`SELECT id, daemon_id, handle, name, role, kind, system_prompt, workspace_root,
		        codex_thread_id, current_turn_id, session_id, status,
		        current_task, current_activity, current_run_id, last_heartbeat_at,
		        last_run_completed, updated_at
		   FROM agents
		  WHERE workspace_id = $1`,
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
			&agent.CodexThreadID,
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
		if agent.CodexThreadID == "" {
			agent.CodexThreadID = agent.SessionID
		}
		if agent.CurrentTurnID == "" {
			agent.CurrentTurnID = agent.CurrentRunID
		}
		s.state.Agents[agent.ID] = agent
	}
	return rows.Err()
}

func (s *Store) loadAgentRunsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id, agent_id, agent_handle, agent_name, agent_kind, system_prompt, session_id,
		        workspace_root, working_dir, prompt, status, desired_status, process_id,
		        launch_time, last_heartbeat_at, completed_at, exit_code, last_message,
		        log_tail, error, assigned_task_ref, updated_at
		   FROM agent_runs
		  WHERE workspace_id = $1`,
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
		`SELECT actor_id, actor_type, document_id, file_path, mode, selection_start, selection_end, activity, updated_at
		   FROM presences
		  WHERE workspace_id = $1`,
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
		`SELECT type, document_id, actor_id, actor_type, summary, occurred_at,
		        provenance_actor_id, provenance_actor_type, provenance_execution_id,
		        provenance_tool, provenance_trigger, provenance_autonomous,
		        provenance_confidence, provenance_requested_by, provenance_source,
		        provenance_intended_scope, provenance_read_set_summary,
		        presence_ref
		   FROM activities
		  WHERE workspace_id = $1
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

func (s *Store) loadThreadsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id, document_id, title, status, anchor_relative_start, anchor_relative_end,
		        anchor_kind, anchor_excerpt, created_by_id, created_by_type,
		        created_by_handle, created_by_name, created_at, updated_at
		   FROM threads
		  WHERE workspace_id = $1`,
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
		`SELECT thread_id, participant_id
		   FROM thread_participants
		  WHERE workspace_id = $1
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
		`SELECT id, thread_id, author_id, author_type, author_handle, author_name, body, kind, created_at
		   FROM thread_messages
		  WHERE workspace_id = $1
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
		`SELECT id, agent_id, agent_handle, type, box, status, document_id, thread_id,
		        thread_message_id, from_update_id, to_update_id,
		        summary, prompt, dedup_key, claimed_by, run_id, last_error, attempt_count,
		        available_at, claimed_at, completed_at, created_at, updated_at
		   FROM agent_events
		  WHERE workspace_id = $1`,
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
