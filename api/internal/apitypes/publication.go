package apitypes

// PublicationIntentDTO summarizes the active write-ahead publication intent of a run (issue
// #1324): what the run will publish when it resumes. It carries no URL, path or credential.
// OriginalHead and Candidate are shortened object ids; Candidate is null until a candidate
// commit exists, and MrIID is null until the merge request exists.
type PublicationIntentDTO struct {
	Branch        string  `json:"branch"`
	MrIID         *int64  `json:"mr_iid"`
	OriginalHead  string  `json:"original_head"`
	Candidate     *string `json:"candidate"`
	State         string  `json:"state"`
	EnrollmentSeq int     `json:"enrollment_seq"`
}
