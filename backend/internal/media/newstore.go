package media

import "fmt"

// StoreOptions is everything NewStore needs to pick and build a backend.
//
// Plain strings rather than an internal/config.Config: internal/media is a
// leaf package that internal/api, internal/export, internal/seed, and both
// commands all depend on, and having it reach back for env-var parsing
// would invert that (docs/CONVENTIONS.md section 2 — main wires, packages
// receive). The cost is that each command copies four fields across; the
// benefit is that internal/media stays testable with literals and usable by
// anything that isn't configured from the environment at all.
type StoreOptions struct {
	// R2 credentials. All four must be set to select the R2 backend;
	// partially-set is an error rather than a silent fallback — see NewStore.
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string

	// MediaDir backs FilesystemStore when R2 is not configured. Expected to
	// be absolute (internal/config.Load guarantees this); NewStore does not
	// re-resolve it, so a caller passing a relative path gets the
	// working-directory-dependent behavior config.resolveMediaDir exists to
	// prevent.
	MediaDir string
}

// NewStore builds the media.Store implied by opts: R2Store when R2
// credentials are present, FilesystemStore otherwise. The second return is a
// short human-readable description of what was selected, for the caller to
// log — every command should say out loud where its bytes are going, since
// "which backend am I actually on?" is the first question any image bug
// raises.
//
// Partially-configured R2 is a hard error, not a fallback to the
// filesystem. Falling back would mean a production deploy with one
// mistyped secret name boots successfully, writes images to a container's
// ephemeral disk, serves them fine until the instance recycles, and then
// 404s — a failure that surfaces hours later and nowhere near its cause.
// Failing at startup puts the error next to the mistake.
func NewStore(opts StoreOptions) (Store, string, error) {
	set := 0
	for _, v := range []string{
		opts.R2AccountID, opts.R2AccessKeyID, opts.R2SecretAccessKey, opts.R2Bucket,
	} {
		if v != "" {
			set++
		}
	}

	switch set {
	case 4:
		s, err := NewR2Store(
			opts.R2AccountID, opts.R2AccessKeyID, opts.R2SecretAccessKey, opts.R2Bucket,
		)
		if err != nil {
			return nil, "", err
		}
		return s, fmt.Sprintf("r2 bucket %q (account %s)", opts.R2Bucket, opts.R2AccountID), nil

	case 0:
		s, err := NewFilesystemStore(opts.MediaDir)
		if err != nil {
			return nil, "", err
		}
		return s, fmt.Sprintf("filesystem %s", opts.MediaDir), nil

	default:
		return nil, "", fmt.Errorf(
			"r2 is partially configured (%d of 4 of R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, "+
				"R2_SECRET_ACCESS_KEY, R2_BUCKET are set): set all four to use R2, "+
				"or none to use the filesystem", set,
		)
	}
}
