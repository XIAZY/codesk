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

	"github.com/reearth/ygo/crdt"
)

const postgresCheckpointInterval = 100
const postgresCheckpointTailLimit = postgresCheckpointInterval

func initPostgresSchemaTables(db *sql.DB) error {
	statements := []string{
		`
		CREATE TABLE IF NOT EXISTS documents (
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			title TEXT NOT NULL,
			client_id_seed BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_documents_workspace_path ON documents (workspace_id, path)`,
		`
		CREATE TABLE IF NOT EXISTS document_heads (
			workspace_id TEXT NOT NULL,
			document_id TEXT PRIMARY KEY,
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
			workspace_id TEXT NOT NULL,
			document_id TEXT NOT NULL,
			update BYTEA NOT NULL,
			actor_id TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_created ON document_updates (workspace_id, document_id, created_at ASC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_document_updates_workspace_document_id ON document_updates (workspace_id, document_id, id ASC)`,
		`
		CREATE TABLE IF NOT EXISTS document_checkpoints (
			id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			document_id TEXT NOT NULL,
			update_id BIGINT NOT NULL,
			crdt_state TEXT NOT NULL,
			state_vector TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (workspace_id, document_id, update_id)
		)
		`,
		`CREATE INDEX IF NOT EXISTS idx_document_checkpoints_workspace_document_update ON document_checkpoints (workspace_id, document_id, update_id DESC)`,
		`UPDATE document_heads h
		    SET update_id = COALESCE((
			    SELECT MAX(u.id)
			      FROM document_updates u
			     WHERE u.workspace_id = h.workspace_id AND u.document_id = h.document_id
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
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_workspace_handle ON users (workspace_id, handle)`,
		`
		CREATE TABLE IF NOT EXISTS agents (
			workspace_id TEXT NOT NULL,
			id TEXT PRIMARY KEY,
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
			anchor JSONB NOT NULL DEFAULT '{}'::jsonb,
			anchor_relative_start TEXT NOT NULL DEFAULT '',
			anchor_relative_end TEXT NOT NULL DEFAULT '',
			anchor_kind TEXT NOT NULL DEFAULT '',
			anchor_start INTEGER NOT NULL DEFAULT 0,
			anchor_end INTEGER NOT NULL DEFAULT 0,
			anchor_line INTEGER NOT NULL DEFAULT 1,
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
		`ALTER TABLE threads ALTER COLUMN anchor SET DEFAULT '{}'::jsonb`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_relative_start TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_relative_end TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_start INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_end INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE threads ADD COLUMN IF NOT EXISTS anchor_line INTEGER NOT NULL DEFAULT 1`,
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
			anchor_start INTEGER NOT NULL,
			anchor_end INTEGER NOT NULL,
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

func (s *Store) loadNormalizedPostgresLocked() error {
	s.state.Documents = map[string]*Document{}
	s.state.Users = map[string]*User{}
	s.state.Agents = map[string]*Agent{}
	s.state.AgentRuns = map[string]*AgentRun{}
	s.state.Presences = map[string]*Presence{}
	s.state.Activities = []*ActivityEvent{}
	s.state.Threads = map[string]*Thread{}
	s.state.AgentEvents = map[string]*AgentEvent{}
	s.state.AgentDocumentViews = map[string]*AgentDocumentView{}
	s.state.DocumentCheckpoints = map[string]*DocumentCheckpoint{}

	if err := s.ensurePostgresCheckpointsLocked(); err != nil {
		return err
	}
	if err := s.loadDocumentsPostgresLocked(); err != nil {
		return err
	}
	if err := s.loadUsersPostgresLocked(); err != nil {
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
		if err = s.replaceAgentEventsPostgresLocked(tx); err != nil {
			return err
		}
		s.dirtyAgentEvents = false
	}
	return tx.Commit()
}

func (s *Store) pendingDocumentMutationIDsLocked() []string {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(s.dirtyDocuments)+len(s.deletedDocuments))
	for documentID := range s.dirtyDocuments {
		if documentID == "" {
			continue
		}
		seen[documentID] = struct{}{}
		ids = append(ids, documentID)
	}
	for documentID := range s.deletedDocuments {
		if documentID == "" {
			continue
		}
		if _, ok := seen[documentID]; ok {
			continue
		}
		ids = append(ids, documentID)
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) persistDocumentsPostgresLocked(tx *sql.Tx) error {
	for documentID := range s.deletedDocuments {
		if _, err := tx.Exec(`DELETE FROM agent_document_views WHERE workspace_id = $1 AND document_id = $2`, s.state.WorkspaceID, documentID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM document_checkpoints WHERE workspace_id = $1 AND document_id = $2`, s.state.WorkspaceID, documentID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM document_updates WHERE workspace_id = $1 AND document_id = $2`, s.state.WorkspaceID, documentID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM document_heads WHERE workspace_id = $1 AND document_id = $2`, s.state.WorkspaceID, documentID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM documents WHERE workspace_id = $1 AND id = $2`, s.state.WorkspaceID, documentID); err != nil {
			return err
		}
	}
	for documentID := range s.dirtyDocuments {
		document := s.state.Documents[documentID]
		if document == nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO documents (workspace_id, id, path, title, client_id_seed, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (id)
			 DO UPDATE SET path = EXCLUDED.path, title = EXCLUDED.title, client_id_seed = EXCLUDED.client_id_seed, updated_at = EXCLUDED.updated_at`,
			s.state.WorkspaceID,
			document.ID,
			document.Path,
			document.Title,
			int64(document.ClientIDSeed),
			document.UpdatedAt,
		); err != nil {
			return err
		}
	}
	latestUpdateByDocument := map[string]int64{}
	for _, event := range s.pendingDocumentEvents {
		var updateID int64
		if err := tx.QueryRow(
			`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id`,
			s.state.WorkspaceID,
			event.DocumentID,
			event.Update,
			event.ActorID,
			event.ActorType,
			event.CreatedAt,
		).Scan(&updateID); err != nil {
			return err
		}
		latestUpdateByDocument[event.DocumentID] = updateID
	}
	for documentID, updateID := range latestUpdateByDocument {
		if document := s.state.Documents[documentID]; document != nil {
			document.UpdateID = updateID
		}
	}
	for documentID := range s.dirtyDocuments {
		document := s.state.Documents[documentID]
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
				  WHERE workspace_id = $4 AND document_id = $5`,
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
	s.deletedDocuments = map[string]struct{}{}
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
		  WHERE workspace_id = $1 AND document_id = $2`,
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
			workspace_id, id, handle, name, role, kind, system_prompt, workspace_root,
			codex_thread_id, current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18
		)
		ON CONFLICT (id)
		DO UPDATE SET handle = EXCLUDED.handle,
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
			workspace_id, id, handle, name, role, kind, system_prompt, workspace_root,
			codex_thread_id, current_turn_id, session_id, status, current_task, current_activity,
			current_run_id, last_heartbeat_at, last_run_completed, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18
		)`,
		workspaceID,
		agent.ID,
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
		anchor, err := json.Marshal(thread.Anchor)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO threads (
				workspace_id, id, document_id, title, status, anchor,
				anchor_relative_start, anchor_relative_end, anchor_kind, anchor_start, anchor_end, anchor_line, anchor_excerpt,
				created_by_id, created_by_type, created_by_handle, created_by_name,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6::jsonb,
				$7, $8, $9, $10, $11, $12, $13,
				$14, $15, $16, $17,
				$18, $19
			)`,
			s.state.WorkspaceID,
			thread.ID,
			thread.DocumentID,
			thread.Title,
			thread.Status,
			string(anchor),
			thread.Anchor.RelativeStart,
			thread.Anchor.RelativeEnd,
			thread.Anchor.Kind,
			thread.Anchor.Start,
			thread.Anchor.End,
			thread.Anchor.Line,
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
				thread_id, thread_message_id, anchor_start, anchor_end, from_update_id, to_update_id, summary,
				prompt, dedup_key, claimed_by, run_id, last_error, attempt_count,
				available_at, claimed_at, completed_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12, $13, $14, $15,
				$16, $17, $18, $19, $20, $21,
				$22, $23, $24, $25, $26
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
			event.AnchorStart,
			event.AnchorEnd,
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
			   AND NOT EXISTS (
			    SELECT 1
			      FROM documents
			     WHERE documents.workspace_id = agent_events.workspace_id
			       AND documents.id = agent_events.document_id
			       AND agent_events.type LIKE 'document.%'
			       AND LOWER(documents.path) LIKE '%.log'
			   )
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
			          agent_events.thread_message_id, agent_events.anchor_start, agent_events.anchor_end,
			          agent_events.from_update_id, agent_events.to_update_id, agent_events.summary,
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
		          thread_message_id, anchor_start, anchor_end, from_update_id, to_update_id,
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
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(document.ClientIDSeed)))
	appliedThrough := int64(0)
	var checkpointState string
	err := db.QueryRow(
		`SELECT update_id, crdt_state
		   FROM document_checkpoints
		  WHERE workspace_id = $1 AND document_id = $2 AND update_id <= $3
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
		  WHERE workspace_id = $1
		    AND document_id = $2
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

func loadDocumentBootstrapUpdatesPostgres(db *sql.DB, workspaceID string, documentID string, headUpdateID int64, clientStateVector []byte) ([][]byte, bool, error) {
	if db == nil || documentID == "" {
		return nil, false, nil
	}
	if headUpdateID <= 0 {
		return nil, true, nil
	}

	updates := [][]byte{}
	appliedThrough := int64(0)
	if len(clientStateVector) > 0 {
		var clientCheckpointUpdateID int64
		clientStateVectorEncoded := base64.StdEncoding.EncodeToString(clientStateVector)
		err := db.QueryRow(
			`SELECT update_id
			   FROM document_checkpoints
			  WHERE workspace_id = $1
			    AND document_id = $2
			    AND update_id <= $3
			    AND state_vector = $4
			  ORDER BY update_id DESC
			  LIMIT 1`,
			workspaceID,
			documentID,
			headUpdateID,
			clientStateVectorEncoded,
		).Scan(&clientCheckpointUpdateID)
		if err != nil && err != sql.ErrNoRows {
			return nil, false, err
		}
		if err == nil {
			appliedThrough = clientCheckpointUpdateID
		}
	}
	if appliedThrough == 0 {
		var checkpointState string
		var checkpointUpdateID int64
		err := db.QueryRow(
			`SELECT update_id, crdt_state
			   FROM document_checkpoints
			  WHERE workspace_id = $1 AND document_id = $2 AND update_id <= $3
			  ORDER BY update_id DESC
			  LIMIT 1`,
			workspaceID,
			documentID,
			headUpdateID,
		).Scan(&checkpointUpdateID, &checkpointState)
		if err != nil && err != sql.ErrNoRows {
			return nil, false, err
		}
		if err == nil && checkpointState != "" {
			checkpoint, decodeErr := base64.StdEncoding.DecodeString(checkpointState)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			updates = append(updates, checkpoint)
			appliedThrough = checkpointUpdateID
		}
	}

	rows, err := db.Query(
		`SELECT update
		   FROM document_updates
		  WHERE workspace_id = $1
		    AND document_id = $2
		    AND id > $3
		    AND id <= $4
		  ORDER BY id ASC`,
		workspaceID,
		documentID,
		appliedThrough,
		headUpdateID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return nil, false, err
		}
		updates = append(updates, append([]byte(nil), update...))
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(updates) == 0 {
		return nil, appliedThrough > 0, nil
	}
	return updates, true, nil
}

func (s *Store) ensurePostgresCheckpointsLocked() error {
	rows, err := s.db.Query(
		`SELECT d.id,
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
		  WHERE d.workspace_id = $1
		    AND h.update_id > 0
		    AND (checkpoint.update_id IS NULL OR h.update_id - checkpoint.update_id > $2)
		  ORDER BY d.path ASC`,
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
		  WHERE workspace_id = $1 AND document_id = $2 AND update_id <= $3
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
		  WHERE workspace_id = $1
		    AND document_id = $2
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

func (s *Store) loadAgentsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT id, handle, name, role, kind, system_prompt, workspace_root,
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

func (s *Store) loadDocumentsPostgresLocked() error {
	rows, err := s.db.Query(
		`SELECT d.id,
		        d.path,
		        d.title,
		        COALESCE(NULLIF(h.state_vector, ''), checkpoint.state_vector, '') AS state_vector,
		        h.update_id,
		        d.updated_at,
		        d.client_id_seed
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
		  WHERE d.workspace_id = $1
		  ORDER BY d.path ASC`,
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
			&document.Path,
			&document.Title,
			&document.StateVector,
			&document.UpdateID,
			&document.UpdatedAt,
			&clientIDSeed,
		); err != nil {
			return err
		}
		document.ClientIDSeed = uint64(clientIDSeed)
		s.state.Documents[document.ID] = document
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
		  WHERE workspace_id = $1 AND document_id = $2 AND update_id <= $3
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
		  WHERE workspace_id = $1
		    AND document_id = $2
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
		`SELECT id, document_id, title, status, anchor_relative_start, anchor_relative_end,
		        anchor_kind, anchor_start, anchor_end, anchor_line, anchor_excerpt, anchor, created_by_id, created_by_type,
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
		        thread_message_id, anchor_start, anchor_end, from_update_id, to_update_id,
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
	var anchorRaw []byte
	if err := scanner.Scan(
		&thread.ID,
		&thread.DocumentID,
		&thread.Title,
		&thread.Status,
		&thread.Anchor.RelativeStart,
		&thread.Anchor.RelativeEnd,
		&thread.Anchor.Kind,
		&thread.Anchor.Start,
		&thread.Anchor.End,
		&thread.Anchor.Line,
		&thread.Anchor.Excerpt,
		&anchorRaw,
		&thread.CreatedByID,
		&thread.CreatedByType,
		&thread.CreatedByHandle,
		&thread.CreatedByName,
		&thread.CreatedAt,
		&thread.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if thread.Anchor.Kind == "" && len(anchorRaw) > 0 {
		if err := json.Unmarshal(anchorRaw, &thread.Anchor); err != nil {
			return nil, err
		}
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
		&event.AnchorStart,
		&event.AnchorEnd,
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
