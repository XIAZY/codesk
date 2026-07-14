package syncer

import "context"
import "path/filepath"
import "testing"

func newWorkspaceReplica(
	_ Config,
	rootDir, actorID, actorType string,
	markDirty func(string),
	markCreate func(localCreateCandidate),
) *workspaceReplica {
	return newWorkspaceReplicaWithFS(rootDir, actorID, actorType, markDirty, markCreate, NewWorkspaceFS(rootDir))
}

func materializeTrackedFile(
	ctx context.Context,
	cache *documentCache,
	document *document,
	absolutePath string,
) (*trackedFile, error) {
	root := workspaceRootForDocumentPath(absolutePath, document.Path)
	return materializeTrackedFileWithFS(ctx, cache, document, absolutePath, NewWorkspaceFS(root))
}

func newTestTrackedFile(t *testing.T, tracked *trackedFile) *trackedFile {
	t.Helper()
	if tracked.WorkspaceRoot == "" {
		tracked.WorkspaceRoot = workspaceRootForDocumentPath(tracked.Path, tracked.DocumentPath)
		if tracked.WorkspaceRoot == "" && tracked.Path != "" {
			tracked.WorkspaceRoot = filepath.Dir(tracked.Path)
		}
	}
	if tracked.FS == nil {
		tracked.FS = NewWorkspaceFS(tracked.WorkspaceRoot)
	}
	return tracked
}
