package dbm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FingerprintForGroups calculates a stable fingerprint for the provided file groups.
// It inspects the current file list managed by DBManager, collects their size and
// modification timestamp, and hashes the aggregated metadata. If none of the
// requested groups contain files, an empty string is returned without error.
func (d *DBManager) FingerprintForGroups(names ...string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}

	type fileFingerprint struct {
		group string
		rel   string
		size  int64
		mod   int64
	}

	entries := make([]fileFingerprint, 0)

	for _, name := range names {
		paths, err := d.GetDBPath(name)
		if err != nil {
			// When the underlying error indicates the files are simply missing,
			// treat it as an empty group rather than a hard failure.
			if strings.Contains(err.Error(), "db file not found") {
				continue
			}
			return "", err
		}

		for _, p := range paths {
			info, statErr := os.Stat(p)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return "", statErr
			}

			rel := p
			if relPath, relErr := filepath.Rel(d.path, p); relErr == nil {
				rel = relPath
			}
			rel = filepath.ToSlash(rel)

			entries = append(entries, fileFingerprint{
				group: name,
				rel:   rel,
				size:  info.Size(),
				mod:   info.ModTime().UnixNano(),
			})
		}
	}

	if len(entries) == 0 {
		return "", nil
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].group == entries[j].group {
			return entries[i].rel < entries[j].rel
		}
		return entries[i].group < entries[j].group
	})

	hasher := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(hasher, "%s|%s|%d|%d;", e.group, e.rel, e.size, e.mod)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
