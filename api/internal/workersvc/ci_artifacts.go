package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CIArtifactDescriptionReserve is enough for a full render, its fixed fence,
// and a separating newline. Composers may reserve this much before admission.
const CIArtifactDescriptionReserve = ciartifacts.RenderMaxBytes + 10

// SetCIArtifactLimits sets repair-specific bounds independently of public jobs.
func (s *Service) SetCIArtifactLimits(files int, bytes int64) {
	s.ciArtifactLimits = (ciartifacts.Limits{MaxFiles: files, MaxBytes: bytes}).Bounded()
}

func artifactKey(id string, ordinal int) string { return fmt.Sprintf("%s:%d", id, ordinal) }

// admitCIArtifacts runs only inside CreateCIRepairRun's create transaction, after
// its repo/schedule/episode locks. Every query uses q; there is no pool acquisition.
// At most 50 distinct files and 16 MiB are sealed, in chunks of JobFileChunkSize.
// A failed insert or seal aborts the entire creation, including sibling files.
func (s *Service) admitCIArtifacts(ctx context.Context, q *store.Queries, run *store.Run, manifest *ciartifacts.Manifest, inputs []ciartifacts.Input) error {
	if manifest == nil {
		if len(inputs) != 0 {
			return errors.New("artifact inputs require a manifest")
		}
		return nil
	}
	m := *manifest
	m.Entries = append([]ciartifacts.Entry{}, m.Entries...)
	if len(inputs) > ciartifacts.MaxFiles+ciartifacts.MaxSkipped || len(m.Entries) > ciartifacts.MaxFiles+ciartifacts.MaxSkipped {
		return errors.New("artifact batch exceeds row bound")
	}
	byKey := map[string]ciartifacts.Input{}
	unique := map[string][]byte{}
	var total int64
	for _, in := range inputs {
		if len(in.Content) == 0 || len(in.Content) > ciartifacts.FileMaxBytes {
			return errors.New("invalid artifact input size")
		}
		key := artifactKey(in.ArtifactID, in.EntryOrdinal)
		if _, exists := byKey[key]; exists {
			return errors.New("duplicate artifact input identity")
		}
		byKey[key] = in
		digest, _ := ciartifacts.Identity(in.Content)
		if _, exists := unique[digest]; !exists {
			total += int64(len(in.Content))
			unique[digest] = in.Content
		}
		if total > ciartifacts.MaxBytes || len(unique) > ciartifacts.MaxFiles {
			return errors.New("artifact batch exceeds hard bound")
		}
	}
	// Verify canonical inputs against their kept source rows first. An alias can
	// resolve only to content verified here, never merely to a claimed digest.
	canonical := map[string][]byte{}
	for _, e := range m.Entries {
		if in, ok := byKey[artifactKey(e.ArtifactID, e.EntryOrdinal)]; ok && e.Status == "kept" {
			digest, _ := ciartifacts.Identity(in.Content)
			canonical[digest] = in.Content
		}
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		in, exists := byKey[artifactKey(e.ArtifactID, e.EntryOrdinal)]
		if e.Status != "kept" {
			if exists {
				return errors.New("input references skipped artifact")
			}
			continue
		}
		if !exists {
			content, ok := canonical[e.SHA256]
			if !ok || e.Size != int64(len(content)) || e.Name != e.SHA256+".txt" {
				return errors.New("kept artifact lacks verified canonical input")
			}
			in.Content = content
		}
		e.SHA256, e.Name = ciartifacts.Identity(in.Content)
		e.Size = int64(len(in.Content))
		e.ManifestID = ""
		e.Reason = ""
		delete(byKey, artifactKey(e.ArtifactID, e.EntryOrdinal))
	}
	if len(byKey) != 0 {
		return errors.New("artifact input missing manifest entry")
	}
	var err error
	m, err = m.Normalize()
	if err != nil {
		return err
	}
	refused := false
	limits := s.ciArtifactLimits.Bounded()
	if len(unique) > limits.MaxFiles || total > limits.MaxBytes {
		refused = true
	}
	if len(unique) > 0 && !refused {
		if s.jobFiles == nil || s.jobFiles.box == nil {
			return ErrJobFilesUnavailable
		}
		if err := store.LockStoredFilesQ(ctx, q, run.UserID); err != nil {
			return err
		}
		sums, err := store.SumStoredFiles(ctx, q, run.UserID, uuid.Nil)
		if err != nil {
			return err
		}
		l := s.jobFiles.limits
		refused = total > l.PerOwnerBytes-sums.OwnerJobBytes || total > l.InstanceBytes-sums.InstanceJobBytes || total > l.StoredFilesBudgetBytes-sums.SharedBytes()
	}
	if refused {
		// Refusal is whole-batch: no reservation or chunk has been inserted.
		// Keep the new refusal reasons ahead of previously skipped entries.
		sort.SliceStable(m.Entries, func(i, j int) bool {
			return m.Entries[i].Status == "kept" && m.Entries[j].Status != "kept"
		})
		rows := make([]ciartifacts.Entry, 0, ciartifacts.MaxSkipped)
		omitted := int64(0)
		if m.Omitted != nil {
			omitted = *m.Omitted
		}
		// A whole-batch refusal also refuses source aliases whose names were
		// omitted during collection. Archive summaries never count as files.
		omitted += m.OmittedKeptAliases
		m.OmittedKeptAliases = 0
		for _, e := range m.Entries {
			if e.Status == "kept" {
				e.Status = "skipped"
				e.Reason = ciartifacts.StorageBudget
				e.ManifestID = ""
				e.Name = ""
				e.SHA256 = ""
				e.Size = 0
			}
			if len(rows) < ciartifacts.MaxSkipped {
				rows = append(rows, e)
			} else if !e.Summary {
				omitted++
			}
		}
		m.Entries = rows
		if omitted > 0 {
			m.Omitted = &omitted
		}
	} else {
		ids := map[string]string{}
		// Manifest order gives deterministic insertion order; duplicate content shares
		// one file and quota charge, while retaining each selected entry's identity.
		for i := range m.Entries {
			e := &m.Entries[i]
			if e.Status != "kept" {
				continue
			}
			if id, exists := ids[e.SHA256]; exists {
				e.ManifestID = id
				continue
			}
			content := unique[e.SHA256]
			f, err := q.ReserveJobFile(ctx, store.ReserveJobFileParams{
				UserID: run.UserID, RunID: pgconv.UUID(run.ID), Direction: JobFileInput,
				DisplayName: e.Name, ByteSize: e.Size, Sha256: pgconv.Text(e.SHA256),
			})
			if err != nil {
				return err
			}
			chunks := int32(0)
			for offset := 0; offset < len(content); offset += JobFileChunkSize {
				end := min(offset+JobFileChunkSize, len(content))
				plain := content[offset:end]
				sealed, err := s.jobFiles.box.SealWithAAD(plain, jobFileChunkAAD(f.ID, run.UserID, int(chunks), len(plain)))
				if err != nil {
					return err
				}
				length := int32(len(plain)) //nolint:gosec // plain is at most JobFileChunkSize (1 MiB).
				if err := q.InsertJobFileChunk(ctx, store.InsertJobFileChunkParams{FileID: f.ID, ChunkIndex: chunks, Length: length, Sealed: sealed}); err != nil {
					return err
				}
				chunks++
			}
			if _, err := q.FinalizeJobFile(ctx, store.FinalizeJobFileParams{ID: f.ID, State: JobFileAttached, ByteSize: e.Size, Sha256: pgconv.Text(e.SHA256), StorageName: pgconv.Text(e.Name), ContentType: pgconv.Text("text/plain"), ChunkCount: chunks}); err != nil {
				return err
			}
			e.ManifestID = f.ID.String()
			ids[e.SHA256] = e.ManifestID
		}
	}
	m, err = m.Normalize()
	if err != nil {
		return err
	}
	// Never crop the opaque existing description: it may contain complete fences.
	// Escaped names cannot contain a triple backtick run; inline name delimiters
	// fit inside the fixed three-backtick fence.
	remaining := MaxIssueDescriptionBytes - len(run.IssueDescription) - 10
	rendered, err := m.RenderWithin(remaining)
	if errors.Is(err, ciartifacts.ErrRenderTooLarge) {
		return fmt.Errorf("%w: CI artifact metadata cannot fit", ErrDescriptionTooLarge)
	}
	if err != nil {
		return err
	}
	description := run.IssueDescription + "\n" + issuedraft.FenceBlock(rendered)
	if len(description) > MaxIssueDescriptionBytes {
		return ErrDescriptionTooLarge
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	saved, err := q.SaveCIArtifactAdmission(ctx, store.SaveCIArtifactAdmissionParams{ID: run.ID, Manifest: raw, Description: description})
	if err != nil {
		return err
	}
	*run = saved
	return nil
}

// ClaimCIRepair is additive to normal guarded prompt claims, never ClaimJob.
type ClaimCIRepair struct {
	Manifest   ciartifacts.Manifest `json:"manifest"`
	InputFiles []ClaimCIRepairFile  `json:"input_files"`
}
type ClaimCIRepairFile struct {
	ManifestID string `json:"manifest_id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

func (s *Service) assembleCIRepair(ctx context.Context, run store.Run) (*ClaimCIRepair, error) {
	if run.Kind != "prompt" || !run.CiConfigGuard || len(run.CiArtifactManifest) == 0 {
		return nil, nil
	}
	var m ciartifacts.Manifest
	if err := json.Unmarshal(run.CiArtifactManifest, &m); err != nil {
		return nil, err
	}
	m, err := m.Normalize()
	if err != nil {
		return nil, err
	}
	files, err := s.q.ListJobInputFilesForClaim(ctx, pgconv.UUID(run.ID))
	if err != nil {
		return nil, err
	}
	out := &ClaimCIRepair{Manifest: m, InputFiles: make([]ClaimCIRepairFile, 0, len(files))}
	for _, f := range files {
		out.InputFiles = append(out.InputFiles, ClaimCIRepairFile{ManifestID: f.ID.String(), Name: f.StorageName.String, Size: f.ByteSize, SHA256: f.Sha256.String})
	}
	return out, nil
}
