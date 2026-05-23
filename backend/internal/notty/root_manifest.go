package notty

import (
	"notty/internal/rootmanifest"
	crdt "notty/internal/ycrdt"
)

const (
	RootManifestTextName = rootmanifest.TextName
	RootManifestMapName  = rootmanifest.MapName
	RootEntryID          = rootmanifest.RootEntryID

	RootEntryKindDir  = rootmanifest.EntryKindDir
	RootEntryKindFile = rootmanifest.EntryKindFile
)

type RootManifest = rootmanifest.Manifest
type RootEntry = rootmanifest.Entry
type RootLocation = rootmanifest.Location
type RootTombstone = rootmanifest.Tombstone
type RootIntent = rootmanifest.Intent
type Projection = rootmanifest.Projection

func NewRootManifest() RootManifest {
	return rootmanifest.New()
}

func NormalizeRootManifestName(name string) string {
	return rootmanifest.NormalizeName(name)
}

func NewRootLocation(parentID string, name string) *RootLocation {
	return rootmanifest.NewLocation(parentID, name)
}

func ReadRootManifest(doc *crdt.Doc) (RootManifest, error) {
	return rootmanifest.Read(doc)
}

func ApplyRootIntents(doc *crdt.Doc, intents []RootIntent) ([]byte, error) {
	return rootmanifest.ApplyIntents(doc, intents)
}

func ValidateRootManifest(previous RootManifest, next RootManifest) error {
	return rootmanifest.Validate(previous, next)
}

func ResolveMaterializedPaths(manifest RootManifest) Projection {
	return rootmanifest.Resolve(manifest)
}

func ConflictPath(filePath string, entryID string) string {
	return rootmanifest.ConflictPath(filePath, entryID)
}

func FirstFreePath(candidate string, entryID string, taken map[string]string) string {
	return rootmanifest.FirstFreePath(candidate, entryID, taken)
}

func ShortID(entryID string) string {
	return rootmanifest.ShortID(entryID)
}

func cloneRootManifest(manifest RootManifest) RootManifest {
	return rootmanifest.Clone(manifest)
}
