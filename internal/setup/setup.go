package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/openpgp/packet"

	"github.com/canonical/chisel/internal/apacheutil"
	"github.com/canonical/chisel/internal/strdist"
)

// Release is a collection of package slices targeting a particular
// distribution version.
type Release struct {
	Path     string
	Packages map[string]*Package
	Archives map[string]*Archive
}

// Archive is the location from which binary packages are obtained.
type Archive struct {
	Name       string
	Version    string
	Suites     []string
	Components []string
	Priority   int
	Pro        string
	PubKeys    []*packet.PublicKey
}

// Package holds a collection of slices that represent parts of themselves.
type Package struct {
	Name    string
	Path    string
	Archive string
	Slices  map[string]*Slice
}

// Slice holds the details about a package slice.
type Slice struct {
	Package   string
	Name      string
	Essential []SliceKey
	Contents  map[string]PathInfo
	Scripts   SliceScripts
}

type SliceScripts struct {
	Mutate string
}

type PathKind string

const (
	DirPath      PathKind = "dir"
	CopyPath     PathKind = "copy"
	GlobPath     PathKind = "glob"
	TextPath     PathKind = "text"
	SymlinkPath  PathKind = "symlink"
	GeneratePath PathKind = "generate"

	// TODO Maybe in the future, for binary support.
	//Base64Path PathKind = "base64"
)

type PathUntil string

const (
	UntilNone   PathUntil = ""
	UntilMutate PathUntil = "mutate"
)

type GenerateKind string

const (
	GenerateNone     GenerateKind = ""
	GenerateManifest GenerateKind = "manifest"
)

type PathInfo struct {
	Kind PathKind
	Info string
	Mode uint

	Mutable  bool
	Until    PathUntil
	Arch     []string
	Generate GenerateKind
	Prefer   string
}

// SameContent returns whether the path has the same content properties as some
// other path. In other words, the resulting file/dir entry is the same. The
// Mutable flag must also match, as that's a common agreement that the actual
// content is not well defined upfront.
func (pi *PathInfo) SameContent(other *PathInfo) bool {
	return (pi.Kind == other.Kind &&
		pi.Info == other.Info &&
		pi.Mode == other.Mode &&
		pi.Mutable == other.Mutable &&
		pi.Generate == other.Generate)
}

type SliceKey = apacheutil.SliceKey

func ParseSliceKey(sliceKey string) (SliceKey, error) {
	return apacheutil.ParseSliceKey(sliceKey)
}

func (s *Slice) String() string { return s.Package + "_" + s.Name }

// Selection holds the required configuration to create a Build for a selection
// of slices from a Release. It's still an abstract proposal in the sense that
// the real information coming from packages is still unknown, so referenced
// paths could potentially be missing, for example.
type Selection struct {
	Release *Release
	Slices  []*Slice
}

// Perfers uses the prefer relationships and returns a map from each path to
// the package where it should be extracted from. If there is no relationship
// for a given path then it will not be present on the map.
func (s *Selection) Prefers() (map[string]*Package, error) {
	prefers, err := s.Release.prefers()
	if err != nil {
		return nil, err
	}

	pathPreferredPkg := make(map[string]*Package)
	for _, slice := range s.Slices {
		for path := range slice.Contents {
			_, hasPrefers := prefers[preferKey{preferSource, path, ""}]
			if !hasPrefers {
				continue
			}
			old, ok := pathPreferredPkg[path]
			if !ok {
				pathPreferredPkg[path] = s.Release.Packages[slice.Package]
				continue
			}
			if old.Name == slice.Package {
				// Skip if the package was already recorded.
				continue
			}
			preferred, err := preferredPathPackage(path, old.Name, slice.Package, prefers)
			if err != nil {
				// Note: we have checked above that the path has prefers and
				// they are different packages so the error cannot be
				// preferNone.
				return nil, err
			}
			pathPreferredPkg[path] = s.Release.Packages[preferred]
		}
	}
	return pathPreferredPkg, nil
}

func ReadRelease(dir string) (*Release, error) {
	logDir := dir
	if strings.Contains(dir, "/.cache/") {
		logDir = filepath.Base(dir)
	}
	logf("Processing %s release...", logDir)

	release, err := readRelease(dir)
	if err != nil {
		return nil, err
	}

	err = release.validate()
	if err != nil {
		return nil, err
	}
	return release, nil
}

func (r *Release) validate() error {
	prefers, err := r.prefers()
	if err != nil {
		return err
	}

	keys := []SliceKey(nil)

	// Check for info conflicts and prepare for following checks. A conflict
	// means that two slices attempt to extract different files or directories
	// to the same location.
	// Conflict validation is done without downloading packages which means that
	// if we are extracting content from different packages to the same location
	// we cannot be sure that it will be the same. On the contrary, content
	// extracted from the same package will never conflict because it is
	// guaranteed to be the same.
	// The above also means that generated content (e.g. text files, directories
	// with make:true) will always conflict with extracted content, because we
	// cannot validate that they are the same without downloading the package.
	paths := make(map[string][]*Slice)
	for _, pkg := range r.Packages {
		for _, new := range pkg.Slices {
			keys = append(keys, SliceKey{pkg.Name, new.Name})
			for newPath, newInfo := range new.Contents {
				if oldSlices, ok := paths[newPath]; ok {
					for _, old := range oldSlices {
						if new.Package != old.Package {
							_, err := preferredPathPackage(newPath, new.Package, old.Package, prefers)
							if err == nil {
								continue
							} else if err != preferNone {
								return err
							}
						}

						oldInfo := old.Contents[newPath]
						if !newInfo.SameContent(&oldInfo) || (newInfo.Kind == CopyPath || newInfo.Kind == GlobPath) && new.Package != old.Package {
							if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
								old, new = new, old
							}
							return fmt.Errorf("slices %s and %s conflict on %s", old, new, newPath)
						}
					}
					paths[newPath] = append(paths[newPath], new)
				} else {
					paths[newPath] = append(paths[newPath], new)
				}
			}
		}
	}

	// Check for invalid prefer relationships where the package does not have
	// the path.
	for skey, source := range prefers {
		if skey.side == preferSource && skey.pkg != "" {
			// Process only the preferSource to traverse the graph only once
			// and avoid repeated work.

			found := false
			for _, slice := range paths[skey.path] {
				if slice.Package == skey.pkg {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("package %s prefers package %q which does not contain path %s", source, skey.pkg, skey.path)
			}
		}
	}

	// Check for glob and generate conflicts.
	for oldPath, oldSlices := range paths {
		for _, old := range oldSlices {
			oldInfo := old.Contents[oldPath]
			if oldInfo.Kind != GeneratePath && oldInfo.Kind != GlobPath {
				break
			}
			for newPath, newSlices := range paths {
				if oldPath == newPath {
					// Identical paths have been filtered earlier.
					continue
				}
				for _, new := range newSlices {
					newInfo := new.Contents[newPath]
					if oldInfo.Kind == GlobPath && (newInfo.Kind == GlobPath || newInfo.Kind == CopyPath) {
						if new.Package == old.Package {
							continue
						}
					}
					if strdist.GlobPath(newPath, oldPath) {
						if (old.Package > new.Package) || (old.Package == new.Package && old.Name > new.Name) ||
							(old.Package == new.Package && old.Name == new.Name && oldPath > newPath) {
							old, new = new, old
							oldPath, newPath = newPath, oldPath
						}
						return fmt.Errorf("slices %s and %s conflict on %s and %s", old, new, oldPath, newPath)
					}
				}
			}
		}
	}

	// Check for cycles.
	_, err = order(r.Packages, keys)
	if err != nil {
		return err
	}

	// Check for archive priority conflicts.
	priorities := make(map[int]*Archive)
	for _, archive := range r.Archives {
		if old, ok := priorities[archive.Priority]; ok {
			if old.Name > archive.Name {
				archive, old = old, archive
			}
			return fmt.Errorf("chisel.yaml: archives %q and %q have the same priority value of %d", old.Name, archive.Name, archive.Priority)
		}
		priorities[archive.Priority] = archive
	}

	// Check that archives pinned in packages are defined.
	for _, pkg := range r.Packages {
		if pkg.Archive == "" {
			continue
		}
		if _, ok := r.Archives[pkg.Archive]; !ok {
			return fmt.Errorf("%s: package refers to undefined archive %q", pkg.Path, pkg.Archive)
		}
	}

	return nil
}

func order(pkgs map[string]*Package, keys []SliceKey) ([]SliceKey, error) {

	// Preprocess the list to improve error messages.
	for _, key := range keys {
		if pkg, ok := pkgs[key.Package]; !ok {
			return nil, fmt.Errorf("slices of package %q not found", key.Package)
		} else if _, ok := pkg.Slices[key.Slice]; !ok {
			return nil, fmt.Errorf("slice %s not found", key)
		}
	}

	// Collect all relevant package slices.
	successors := map[string][]string{}
	pending := append([]SliceKey(nil), keys...)

	seen := make(map[SliceKey]bool)
	for i := 0; i < len(pending); i++ {
		key := pending[i]
		if seen[key] {
			continue
		}
		seen[key] = true
		pkg := pkgs[key.Package]
		slice := pkg.Slices[key.Slice]
		fqslice := slice.String()
		predecessors := successors[fqslice]
		for _, req := range slice.Essential {
			fqreq := req.String()
			if reqpkg, ok := pkgs[req.Package]; !ok || reqpkg.Slices[req.Slice] == nil {
				return nil, fmt.Errorf("%s requires %s, but slice is missing", fqslice, fqreq)
			}
			predecessors = append(predecessors, fqreq)
		}
		successors[fqslice] = predecessors
		pending = append(pending, slice.Essential...)
	}

	// Sort them up.
	var order []SliceKey
	for _, names := range tarjanSort(successors) {
		if len(names) > 1 {
			return nil, fmt.Errorf("essential loop detected: %s", strings.Join(names, ", "))
		}
		name := names[0]
		dot := strings.IndexByte(name, '_')
		order = append(order, SliceKey{name[:dot], name[dot+1:]})
	}

	return order, nil
}

func readRelease(baseDir string) (*Release, error) {
	baseDir = filepath.Clean(baseDir)
	filePath := filepath.Join(baseDir, "chisel.yaml")
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read release definition: %s", err)
	}
	release, err := parseRelease(baseDir, filePath, data)
	if err != nil {
		return nil, err
	}
	err = readSlices(release, baseDir, filepath.Join(baseDir, "slices"))
	if err != nil {
		return nil, err
	}
	return release, err
}

func readSlices(release *Release, baseDir, dirName string) error {
	entries, err := os.ReadDir(dirName)
	if err != nil {
		return fmt.Errorf("cannot read %s%c directory", stripBase(baseDir, dirName), filepath.Separator)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			err := readSlices(release, baseDir, filepath.Join(dirName, entry.Name()))
			if err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		match := apacheutil.FnameExp.FindStringSubmatch(entry.Name())
		if match == nil {
			return fmt.Errorf("invalid slice definition filename: %q", entry.Name())
		}

		pkgName := match[1]
		pkgPath := filepath.Join(dirName, entry.Name())
		if pkg, ok := release.Packages[pkgName]; ok {
			return fmt.Errorf("package %q slices defined more than once: %s and %s\")", pkgName, pkg.Path, pkgPath)
		}
		data, err := os.ReadFile(pkgPath)
		if err != nil {
			// Errors from package os generally include the path.
			return fmt.Errorf("cannot read slice definition file: %v", err)
		}

		pkg, err := parsePackage(baseDir, pkgName, stripBase(baseDir, pkgPath), data)
		if err != nil {
			return err
		}

		release.Packages[pkg.Name] = pkg
	}
	return nil
}

func stripBase(baseDir, path string) string {
	// Paths must be clean for this to work correctly.
	return strings.TrimPrefix(path, baseDir+string(filepath.Separator))
}

func Select(release *Release, slices []SliceKey) (*Selection, error) {
	logf("Selecting slices...")

	selection := &Selection{
		Release: release,
	}

	sorted, err := order(release.Packages, slices)
	if err != nil {
		return nil, err
	}
	selection.Slices = make([]*Slice, len(sorted))
	for i, key := range sorted {
		selection.Slices[i] = release.Packages[key.Package].Slices[key.Slice]
	}

	for _, new := range selection.Slices {
		for newPath, newInfo := range new.Contents {
			// An invalid "generate" value should only throw an error if that
			// particular slice is selected. Hence, the check is here.
			switch newInfo.Generate {
			case GenerateNone, GenerateManifest:
			default:
				return nil, fmt.Errorf("slice %s has invalid 'generate' for path %s: %q",
					new, newPath, newInfo.Generate)
			}
		}
	}

	return selection, nil
}

const (
	preferSource = 1
	preferTarget = 2
)

type preferKey struct {
	side int
	path string
	pkg  string
}

func (r *Release) prefers() (map[preferKey]string, error) {
	prefers := make(map[preferKey]string)
	for _, pkg := range r.Packages {
		for _, slice := range pkg.Slices {
			for path, info := range slice.Contents {
				if info.Prefer != "" {
					if _, ok := r.Packages[info.Prefer]; !ok {
						return nil, fmt.Errorf("slice %s path %s 'prefer' refers to undefined package %q", slice, path, info.Prefer)
					}
					tkey := preferKey{preferTarget, path, pkg.Name}
					skey := preferKey{preferSource, path, info.Prefer}
					if target, ok := prefers[tkey]; ok {
						if target != info.Prefer {
							pkg1, pkg2 := sortPair(target, info.Prefer)
							return nil, fmt.Errorf("package %q has conflicting prefers for %s: %s != %s",
								pkg.Name, path, pkg1, pkg2)
						}
					} else if source, ok := prefers[skey]; ok {
						if source != pkg.Name {
							pkg1, pkg2 := sortPair(source, pkg.Name)
							return nil, fmt.Errorf("packages %q and %q cannot both prefer %q for %s",
								pkg1, pkg2, info.Prefer, path)
						}
					} else {
						prefers[tkey] = info.Prefer
						prefers[skey] = pkg.Name
						// Sample package that requires this path to be in a prefer relationship.
						prefers[preferKey{preferSource, path, ""}] = pkg.Name
					}
				}
			}
		}
	}
	return prefers, nil
}

// preferredPathPackage returns pkg1 if it can be reached from pkg2 following
// prefer relationships, and conversely for pkg2. If none are reachable it
// returns the preferNone error.
//
// If there is a cycle, an error is returned.
func preferredPathPackage(path, pkg1, pkg2 string, prefers map[preferKey]string) (choice string, err error) {
	pkg1, pkg2 = sortPair(pkg1, pkg2)
	prefer1, err := findPrefer(path, pkg2, pkg1, prefers)
	if err != nil {
		return "", err
	}
	prefer2, err := findPrefer(path, pkg1, pkg2, prefers)
	if err != nil {
		return "", err
	}
	if prefer1 && prefer2 {
		return "", fmt.Errorf("package %q is part of a prefer loop on %s", pkg1, path)
	} else if prefer1 {
		return pkg1, nil
	} else if prefer2 {
		return pkg2, nil
	}
	sample, enforce := prefers[preferKey{preferSource, path, ""}]
	if enforce {
		conflict := pkg1
		if conflict == sample {
			conflict = pkg2
		}
		pkg1, pkg2 = sortPair(conflict, sample)
		return "", fmt.Errorf("package %q and %q conflict on %s without prefer relationship", pkg1, pkg2, path)
	}
	return "", preferNone
}

var preferNone = errors.New("no prefer relationship")

func findPrefer(path, pkg, prefer string, prefers map[preferKey]string) (found bool, err error) {
	if len(prefers) == 0 {
		return false, nil
	}
	// This logic is optimized for the happy case, which is
	// always the case unless the release is broken. Note that
	// the pkg reported in the error is the one inside the loop,
	// not necessarily the input parameter.
	for i := 0; i < len(prefers); i++ {
		pkg = prefers[preferKey{preferTarget, path, pkg}]
		if pkg == "" {
			return false, nil
		}
		if pkg == prefer {
			return true, nil
		}
	}
	return false, fmt.Errorf("package %q is part of a prefer loop on %s", pkg, path)
}

func sortPair(name1, name2 string) (sorted1, sorted2 string) {
	if name1 < name2 {
		return name1, name2
	}
	return name2, name1
}
