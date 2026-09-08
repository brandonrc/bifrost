// Private storage catalog (requirement 12, plan ruling D7): administrators
// catalog storage sources as named entries in the policy row; a spec names
// entries; the provisioner projects them onto the pods as
// `envFrom.secretRef` (env, secret source), a read-only volume at the
// entry's mount path (file) backed by a Secret, a PersistentVolumeClaim or
// a hostPath. A Secret's contents never cross Bifrost: the API carries
// names and paths only, and the resolution persisted on a spec is delivery
// instructions, not data.
//
// The predecessor's pod-shaping rule applies: the catalog is validated as
// a unit at the edit, a spec is resolved once at admission, and a later
// catalog edit is never retroactive (the resolution is stored on the spec).
package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/brandonrc/bifrost/internal/core"
)

// reservedMountPrefixes are the mount points a file-mode entry may neither
// take nor shadow: the image's own root, its scratch space and Ray's home
// (where ray start writes session state). A Secret mounted over any of
// them replaces the image's directory with the Secret's keys and the pod
// never starts.
var reservedMountPrefixes = []string{"/", "/tmp", "/home/ray"}

// maxStorageNameLen keeps "storage-<name>" a valid volume name (a DNS
// label, 63 chars).
const maxStorageNameLen = 63 - len("storage-")

// isStorageName reports whether s is usable as a catalog name: an RFC 1123
// label (the provisioner derives the Secret volume's name from it).
func isStorageName(s string) bool {
	return core.IsK8sName(s) && !strings.Contains(s, ".") && len(s) <= maxStorageNameLen
}

// mountPathReserved reports whether p is, or lies under, a reserved mount
// prefix. Paths are compared after cleaning trailing slashes so "/tmp/"
// and "/tmp" are the same mount point.
func mountPathReserved(p string) bool {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return true // "/" (or "///")
	}
	for _, r := range reservedMountPrefixes {
		if r == "/" {
			continue
		}
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// hostPathTypes are the Kubernetes HostPathType values a host_path entry
// may declare ("" = no node-path type checking). Mirrored here rather than
// imported from k8s.io/api so the API edge does not depend on the
// provisioner's dependency set.
var hostPathTypes = map[string]bool{
	"": true, "DirectoryOrCreate": true, "Directory": true, "FileOrCreate": true,
	"File": true, "Socket": true, "CharDevice": true, "BlockDevice": true,
}

// storageEntryToWire converts one catalog entry for PolicyView. Only the
// name, the source's names/paths and the delivery mode are ever on the
// wire — a Secret's data is not a thing Bifrost can read, let alone echo.
func storageEntryToWire(e *core.StorageEntry) StorageEntry {
	projects := make([]string, len(e.Projects))
	copy(projects, e.Projects)
	source := StorageEntrySource(e.Source.OrDefault())
	return StorageEntry{
		Name:       e.Name,
		Source:     &source,
		SecretName: strPtrOrNil(e.SecretName),
		ClaimName:  strPtrOrNil(e.ClaimName),
		HostPath:   strPtrOrNil(e.HostPath),
		HostType:   strPtrOrNil(e.HostType),
		Mode:       StorageEntryMode(e.Mode),
		MountPath:  e.MountPath,
		Projects:   &projects,
	}
}

// strPtrOrNil maps an empty optional field onto nil so the wire carries
// `null`, never `""`, for a field the entry's source does not use.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// storageToWire never returns nil: the contract's catalog is `[]`, not
// `null`, when empty.
func storageToWire(in []core.StorageEntry) []StorageEntry {
	out := make([]StorageEntry, 0, len(in))
	for i := range in {
		out = append(out, storageEntryToWire(&in[i]))
	}
	return out
}

// storageFromWire converts an incoming catalog and validates it as a
// unit, refusing the edit with a precise 400 rather than letting every
// later create fail (the Rust predecessor's rule: validate at the edit). Checks: unique
// RFC 1123 names; a known source; the source's own required name/path
// (secret_name / claim_name RFC 1123, host_path absolute, host_type a
// known HostPathType) with no other source's fields set; a known mode —
// `file` for the volume sources, which have no keys to inject; for file
// mode a mount_path that is absolute, unique across the catalog and
// neither a reserved path nor under one; no mount_path for env mode;
// non-empty project names.
func storageFromWire(in []StorageEntry) ([]core.StorageEntry, error) {
	out := make([]core.StorageEntry, 0, len(in))
	seen := make(map[string]bool, len(in))
	mounts := make(map[string]string, len(in))
	for i := range in {
		w := &in[i]
		what := fmt.Sprintf("invalid storage entry %q: ", w.Name)
		if w.Name == "" {
			return nil, badRequest(fmt.Sprintf("invalid storage entry at index %d: name must not be empty", i))
		}
		if !isStorageName(w.Name) {
			return nil, badRequest(what + "name must be an RFC 1123 label (lowercase alphanumerics and '-', at most 55 characters)")
		}
		if seen[w.Name] {
			return nil, badRequest(what + "duplicate name")
		}
		seen[w.Name] = true
		e := core.StorageEntry{Name: w.Name, Mode: core.StorageMode(w.Mode)}
		src := core.DefaultStorageSource
		if w.Source != nil {
			src = core.StorageSource(*w.Source)
		}
		// fieldForSource rejects a set field that belongs to another
		// source, so a catalog cannot carry a Secret name a volume entry
		// would silently ignore.
		fieldForSource := func(p *string, field, source string) error {
			if p != nil && *p != "" {
				return badRequest(fmt.Sprintf("%s%s is only valid for source %q", what, field, source))
			}
			return nil
		}
		switch src {
		case core.StorageSourceSecret:
			if w.SecretName == nil || *w.SecretName == "" {
				return nil, badRequest(what + "secret_name is required for source \"secret\"")
			}
			if !core.IsK8sName(*w.SecretName) {
				return nil, badRequest(what + "secret_name must be a valid Kubernetes Secret name (RFC 1123)")
			}
			e.SecretName = *w.SecretName
			for _, f := range []struct {
				p    *string
				name string
			}{{w.ClaimName, "claim_name"}, {w.HostPath, "host_path"}, {w.HostType, "host_type"}} {
				if err := fieldForSource(f.p, f.name, "persistent_volume_claim/host_path"); err != nil {
					return nil, err
				}
			}
		case core.StorageSourcePersistentVolumeClaim:
			if w.ClaimName == nil || *w.ClaimName == "" {
				return nil, badRequest(what + "claim_name is required for source \"persistent_volume_claim\"")
			}
			if !core.IsK8sName(*w.ClaimName) {
				return nil, badRequest(what + "claim_name must be a valid PersistentVolumeClaim name (RFC 1123)")
			}
			e.ClaimName = *w.ClaimName
			for _, f := range []struct {
				p    *string
				name string
			}{{w.SecretName, "secret_name"}, {w.HostPath, "host_path"}, {w.HostType, "host_type"}} {
				if err := fieldForSource(f.p, f.name, "secret/host_path"); err != nil {
					return nil, err
				}
			}
		case core.StorageSourceHostPath:
			if w.HostPath == nil || *w.HostPath == "" {
				return nil, badRequest(what + "host_path is required for source \"host_path\"")
			}
			if !strings.HasPrefix(*w.HostPath, "/") {
				return nil, badRequest(what + "host_path must be absolute")
			}
			e.HostPath = *w.HostPath
			if w.HostType != nil {
				if !hostPathTypes[*w.HostType] {
					return nil, badRequest(fmt.Sprintf("%shost_type %q is not a Kubernetes HostPathType", what, *w.HostType))
				}
				e.HostType = *w.HostType
			}
			for _, f := range []struct {
				p    *string
				name string
			}{{w.SecretName, "secret_name"}, {w.ClaimName, "claim_name"}} {
				if err := fieldForSource(f.p, f.name, "secret/persistent_volume_claim"); err != nil {
					return nil, err
				}
			}
		default:
			return nil, badRequest(fmt.Sprintf("%ssource must be \"secret\", \"persistent_volume_claim\" or \"host_path\"", what))
		}
		e.Source = src
		if src != core.StorageSourceSecret && e.Mode == core.StorageModeEnv {
			return nil, badRequest(what + "mode \"env\" is only valid for source \"secret\" (a volume has no keys to inject)")
		}
		switch e.Mode {
		case core.StorageModeEnv:
			if w.MountPath != nil && *w.MountPath != "" {
				return nil, badRequest(what + "mount_path is only valid for mode \"file\"")
			}
		case core.StorageModeFile:
			if w.MountPath == nil || *w.MountPath == "" {
				return nil, badRequest(what + "mount_path is required for mode \"file\"")
			}
			p := *w.MountPath
			if !strings.HasPrefix(p, "/") {
				return nil, badRequest(what + "mount_path must be absolute")
			}
			if mountPathReserved(p) {
				return nil, badRequest(fmt.Sprintf("%smount_path %q is reserved (must not be, or lie under, %s)",
					what, p, strings.Join(reservedMountPrefixes, ", ")))
			}
			key := strings.TrimRight(p, "/")
			if other, dup := mounts[key]; dup {
				return nil, badRequest(fmt.Sprintf("%smount_path %q is already used by entry %q", what, p, other))
			}
			mounts[key] = w.Name
			mp := p
			e.MountPath = &mp
		default:
			return nil, badRequest(fmt.Sprintf("%smode must be \"env\" or \"file\"", what))
		}
		if w.Projects != nil {
			for _, proj := range *w.Projects {
				if proj == "" {
					return nil, badRequest(what + "projects must not contain an empty name")
				}
			}
			e.Projects = append([]string(nil), (*w.Projects)...)
		}
		out = append(out, e)
	}
	return out, nil
}

// storageAvailableTo reports whether project may reference e: an empty
// project list means every project.
func storageAvailableTo(e *core.StorageEntry, project string) bool {
	if len(e.Projects) == 0 {
		return true
	}
	for _, p := range e.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// resolveStorage resolves names against the effective policy's storage
// catalog for project, in the order given. Every name must exist and be
// available to the project — an unknown or foreign name is a 400 (never a
// workload that silently runs without its credentials); a duplicate name
// is a 400 too (one Secret would be mounted twice). nil names resolve to
// nil. Store failures surface as 5xx through wrapStoreErr.
func (s *Server) resolveStorage(ctx context.Context, project string, names []string) ([]core.ResolvedStorage, error) {
	if len(names) == 0 {
		return nil, nil
	}
	p, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	var catalog []core.StorageEntry
	if p != nil {
		catalog = p.Storage
	}
	out := make([]core.ResolvedStorage, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return nil, badRequest(fmt.Sprintf("storage %q is listed twice", name))
		}
		seen[name] = true
		var entry *core.StorageEntry
		for i := range catalog {
			if catalog[i].Name == name {
				entry = &catalog[i]
				break
			}
		}
		if entry == nil {
			return nil, badRequest(fmt.Sprintf("no such storage %q", name))
		}
		if !storageAvailableTo(entry, project) {
			return nil, badRequest(fmt.Sprintf("storage %q is not available to project %q", name, project))
		}
		r := core.ResolvedStorage{
			Name: entry.Name, Source: entry.Source.OrDefault(), SecretName: entry.SecretName,
			ClaimName: entry.ClaimName, HostPath: entry.HostPath, HostType: entry.HostType, Mode: entry.Mode,
		}
		if entry.MountPath != nil {
			mp := *entry.MountPath
			r.MountPath = &mp
		}
		out = append(out, r)
	}
	return out, nil
}
