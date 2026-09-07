package journal

import "time"

type Manifest struct {
	SchemaVersion            int      `json:"schemaVersion"`
	Version                  string   `json:"version"`
	OCIRepository            string   `json:"ociRepository"`
	Digest                   string   `json:"digest"`
	PluginCommitSHA          string   `json:"pluginCommitSha"`
	ToolsSHA                 string   `json:"lepinoidToolsSha256"`
	MultiverseSHA            string   `json:"multiverseSha256"`
	MultiverseVersion        string   `json:"multiverseVersion"`
	SupportedMinecraft       []string `json:"supportedMinecraft"`
	ConfigMapResourceVersion string   `json:"configMapResourceVersion"`
}

type Current struct {
	Manifest
	InstalledAt time.Time `json:"installedAt"`
}

type Jar struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
type Pair struct {
	Tools      Jar `json:"lepinoidTools"`
	Multiverse Jar `json:"multiverseCore"`
}

type Maintenance struct {
	WhitelistBackup   *string    `json:"whitelistJsonBackupBase64"`
	WhitelistChecksum *string    `json:"whitelistJsonChecksum"`
	WhitelistExisted  *bool      `json:"whitelistJsonExisted"`
	EntriesSHA        *string    `json:"whitelistEntriesSha256"`
	RuntimeEnabled    *bool      `json:"runtimeWhitelistEnabled"`
	PersistedEnabled  *bool      `json:"persistedWhitelistEnabled"`
	StartedAt         *time.Time `json:"startedAt"`
}

type Journal struct {
	SchemaVersion              int         `json:"schemaVersion"`
	TransactionID              string      `json:"transactionId"`
	Phase                      string      `json:"phase"`
	Lifecycle                  string      `json:"lifecycleState"`
	Outcome                    *string     `json:"outcome"`
	Resolution                 *string     `json:"resolution"`
	MaintenanceRequired        bool        `json:"maintenanceRequired"`
	AccessState                string      `json:"accessState"`
	CommitCandidate            *string     `json:"commitCandidate"`
	SuspendReason              *string     `json:"suspendReason"`
	FailureReason              *string     `json:"failureReason"`
	FencingGeneration          int64       `json:"fencingGeneration"`
	ExpectedPodUID             string      `json:"expectedPodUid"`
	TargetManifest             Manifest    `json:"targetManifest"`
	SourceDigest               string      `json:"sourceDigest"`
	TargetDigest               string      `json:"targetDigest"`
	Source                     Pair        `json:"source"`
	Target                     Pair        `json:"target"`
	StagingPath                string      `json:"stagingPath"`
	BackupPath                 string      `json:"backupPath"`
	RestartRequestedAt         *time.Time  `json:"restartRequestedAt"`
	ExpectedTemplateGeneration *int64      `json:"expectedPodTemplateGeneration"`
	ReplacementPodUID          *string     `json:"replacementPodUid"`
	PreviousPodUID             *string     `json:"previousPodUid"`
	Maintenance                Maintenance `json:"maintenance"`
}

type Flag struct {
	SchemaVersion     int       `json:"schemaVersion"`
	TransactionID     string    `json:"transactionId"`
	CreatedAt         time.Time `json:"createdAt"`
	JournalPath       string    `json:"journalPath"`
	FencingGeneration int64     `json:"fencingGeneration"`
}

type Checksums struct {
	Tools      *string `json:"lepinoidTools"`
	Multiverse *string `json:"multiverseCore"`
}
type Recovery struct {
	SchemaVersion    int        `json:"schemaVersion"`
	TransactionID    *string    `json:"transactionId"`
	Generation       int64      `json:"generation"`
	PodUID           *string    `json:"podUid"`
	ObservedPhase    *string    `json:"observedPhase"`
	Before           Checksums  `json:"jarChecksumsBefore"`
	After            *Checksums `json:"jarChecksumsAfter"`
	PersistedBefore  bool       `json:"persistedWhitelistBefore"`
	PersistedAfter   bool       `json:"persistedWhitelistAfter"`
	FlagBefore       bool       `json:"flagBefore"`
	FlagAfter        bool       `json:"flagAfter"`
	GenerationBefore int64      `json:"fencingGenerationBefore"`
	GenerationAfter  int64      `json:"fencingGenerationAfter"`
	Result           string     `json:"result"`
	ConsumedByJobUID *string    `json:"consumedByJobUid"`
	Timestamp        time.Time  `json:"timestamp"`
}

type Blocking struct {
	SchemaVersion      int       `json:"schemaVersion"`
	Kind               string    `json:"kind"`
	Reason             string    `json:"reason"`
	Detail             string    `json:"detail"`
	ObservedJournalIDs []string  `json:"observedJournalIds"`
	DetectedAt         time.Time `json:"detectedAt"`
	DetectedByJobUID   string    `json:"detectedByJobUid"`
}

type Metadata struct {
	SchemaVersion int        `json:"schemaVersion"`
	Digest        string     `json:"digest"`
	StagedAt      *time.Time `json:"stagedAt"`
	BackedUpAt    *time.Time `json:"backedUpAt"`
	Pair
}
