package setup

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/canonical/chisel/internal/strdist"
)

// Release is a collection of package slices targeting a particular
// distribution version.
type Release struct {
	Path           string
	Packages       map[string]*Package
	Archives       map[string]*Archive
	DefaultArchive string
}

// Archive is the location from which binary packages are obtained.
type Archive struct {
	Name       string
	Version    string
	Components []string
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
	DirPath     PathKind = "dir"
	CopyPath    PathKind = "copy"
	GlobPath    PathKind = "glob"
	TextPath    PathKind = "text"
	SymlinkPath PathKind = "symlink"

	// TODO Maybe in the future, for binary support.
	//Base64Path PathKind = "base64"
)

type PathInfo struct {
	Kind PathKind
	Info string
	Mode uint

	Mutable bool
}

type SliceKey struct {
	Package string
	Slice   string
}

func (s *Slice) String() string   { return s.Package + "_" + s.Name }
func (s SliceKey) String() string { return s.Package + "_" + s.Slice }

// Selection holds the required configuration to create a Build for a selection
// of slices from a Release. It's still an abstract proposal in the sense that
// the real information coming from pacakges is still unknown, so referenced
// paths could potentially be missing, for example.
type Selection struct {
	Release *Release
	Slices  []*Slice
}

func ReadRelease(dir string) (*Release, error) {
	logDir := dir
	if strings.Contains(dir, "/.cache/") {
		logDir = filepath.Base(dir)
	}
	logf("Processing %s release...", logDir)

	release := &Release{
		Path:     dir,
		Packages: make(map[string]*Package),
	}

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
	keys := []SliceKey(nil)
	paths := make(map[string]*Slice)
	globs := make(map[string]*Slice)

	// Check for info conflicts and prepare for following checks.
	for _, pkg := range r.Packages {
		for _, new := range pkg.Slices {
			keys = append(keys, SliceKey{pkg.Name, new.Name})
			for newPath, newInfo := range new.Contents {
				if old, ok := paths[newPath]; ok {
					oldInfo := old.Contents[newPath]
					if newInfo != oldInfo || (newInfo.Kind == CopyPath || newInfo.Kind == GlobPath) && new.Package != old.Package {
						if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
							old, new = new, old
						}
						return fmt.Errorf("slices %s and %s conflict on %s", old, new, newPath)
					}
				} else {
					if newInfo.Kind == GlobPath {
						globs[newPath] = new
					}
					paths[newPath] = new
				}
			}
		}
	}

	// Check for cycles.
	_, err := order(r.Packages, keys)
	if err != nil {
		return err
	}

	// Check for glob conflicts.
	for newPath, new := range globs {
		for oldPath, old := range paths {
			if new.Package == old.Package {
				continue
			}
			if strdist.GlobPath(newPath, oldPath) {
				if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
					old, oldPath, new, newPath = new, newPath, old, oldPath
				}
				return fmt.Errorf("slices %s and %s conflict on %s and %s", old, new, oldPath, newPath)
			}
		}
		paths[newPath] = new
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

var fnameExp = regexp.MustCompile(`^([a-z0-9](?:-?[.a-z0-9+]){2,})\.yaml$`)
var snameExp = regexp.MustCompile(`^([a-z](?:-?[a-z0-9]){2,})$`)
var knameExp = regexp.MustCompile(`^([a-z0-9](?:-?[.a-z0-9+]){2,})_([a-z](?:-?[a-z0-9]){2,})$`)

func ParseSliceKey(sliceKey string) (SliceKey, error) {
	match := knameExp.FindStringSubmatch(sliceKey)
	if match == nil {
		return SliceKey{}, fmt.Errorf("invalid slice reference: %q", sliceKey)
	}
	return SliceKey{match[1], match[2]}, nil
}

func readRelease(baseDir string) (*Release, error) {
	baseDir = filepath.Clean(baseDir)
	filePath := filepath.Join(baseDir, "chisel.yaml")
	data, err := ioutil.ReadFile(filePath)
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
	finfos, err := ioutil.ReadDir(dirName)
	if err != nil {
		return fmt.Errorf("cannot read %s%c directory", stripBase(baseDir, dirName), filepath.Separator)
	}

	for _, finfo := range finfos {
		if finfo.IsDir() {
			err := readSlices(release, baseDir, filepath.Join(dirName, finfo.Name()))
			if err != nil {
				return err
			}
			continue
		}
		if finfo.IsDir() || !strings.HasSuffix(finfo.Name(), ".yaml") {
			continue
		}
		match := fnameExp.FindStringSubmatch(finfo.Name())
		if match == nil {
			return fmt.Errorf("invalid slice definition filename: %q\")", finfo.Name())
		}

		pkgName := match[1]
		pkgPath := filepath.Join(dirName, finfo.Name())
		if pkg, ok := release.Packages[pkgName]; ok {
			return fmt.Errorf("package %q slices defined more than once: %s and %s\")", pkgName, pkg.Path, pkgPath)
		}
		data, err := ioutil.ReadFile(pkgPath)
		if err != nil {
			// Errors from package os generally include the path.
			return fmt.Errorf("cannot read slice definition file: %v", err)
		}

		pkg, err := parsePackage(baseDir, pkgName, stripBase(baseDir, pkgPath), data)
		if err != nil {
			return err
		}
		if pkg.Archive == "" {
			pkg.Archive = release.DefaultArchive
		}

		release.Packages[pkg.Name] = pkg
	}
	return nil
}

type yamlRelease struct {
	Format   string                 `yaml:"format"`
	Archives map[string]yamlArchive `yaml:"archives`
}

const yamlReleaseFormat = "chisel-v1"

type yamlArchive struct {
	Version    string   `yaml:"version"`
	Components []string `yaml:"components"`
	Default    bool     `yaml:"default"`
}

type yamlPackage struct {
	Name    string               `yaml:"package"`
	Archive string               `yaml:"archive"`
	Slices  map[string]yamlSlice `yaml:"slices"`
}

type yamlPath struct {
	Dir     bool   `yaml:"make"`
	Mode    uint   `yaml:"mode"`
	Copy    string `yaml:"copy"`
	Text    string `yaml:"text"`
	Symlink string `yaml:"symlink"`
	Mutable bool   `yaml:"mutable"`
}

type yamlSlice struct {
	Essential []string             `yaml:"essential"`
	Contents  map[string]*yamlPath `yaml:"contents"`
	Mutate    string               `yaml:"mutate"`
}

func parseRelease(baseDir, filePath string, data []byte) (*Release, error) {
	release := &Release{
		Path:     baseDir,
		Packages: make(map[string]*Package),
		Archives: make(map[string]*Archive),
	}

	fileName := stripBase(baseDir, filePath)

	yamlVar := yamlRelease{}
	dec := yaml.NewDecoder(bytes.NewBuffer(data))
	dec.KnownFields(true)
	err := dec.Decode(&yamlVar)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot parse release definition: %v", fileName, err)
	}
	if yamlVar.Format != yamlReleaseFormat {
		return nil, fmt.Errorf("%s: expected format %q, got %q", fileName, yamlReleaseFormat, yamlVar.Format)
	}
	if len(yamlVar.Archives) == 0 {
		return nil, fmt.Errorf("%s: no archives defined", fileName)
	}
	if len(yamlVar.Archives) > 1 {
		return nil, fmt.Errorf("%s: multiple archives not yet supported", fileName)
	}

	for archiveName, details := range yamlVar.Archives {
		const ubuntuArchive = "ubuntu"
		if archiveName != ubuntuArchive {
			return nil, fmt.Errorf("%s: only %q archives are supported for now", fileName, ubuntuArchive)
		}
		if details.Version == "" {
			return nil, fmt.Errorf("%s: archive %q missing version field", fileName, archiveName)
		}
		if len(details.Components) == 0 {
			return nil, fmt.Errorf("%s: archive %q missing components field", fileName, archiveName)
		}
		if len(yamlVar.Archives) == 1 {
			details.Default = true
		} else if details.Default && release.DefaultArchive != "" {
			return nil, fmt.Errorf("%s: more than one default archive: %s, %s", fileName, release.DefaultArchive, archiveName)
		}
		if details.Default {
			release.DefaultArchive = archiveName
		}
		release.Archives[archiveName] = &Archive{
			Name:       archiveName,
			Version:    details.Version,
			Components: details.Components,
		}
	}

	return release, err
}

func parsePackage(baseDir, pkgName, pkgPath string, data []byte) (*Package, error) {
	pkg := Package{
		Name:   pkgName,
		Path:   pkgPath,
		Slices: make(map[string]*Slice),
	}

	yamlPkg := yamlPackage{}
	dec := yaml.NewDecoder(bytes.NewBuffer(data))
	dec.KnownFields(true)
	err := dec.Decode(&yamlPkg)
	if err != nil {
		return nil, fmt.Errorf("cannot parse package %q slice definitions: %v", pkgName, err)
	}
	if yamlPkg.Name != pkg.Name {
		return nil, fmt.Errorf("%s: filename and 'package' field (%q) disagree", pkgPath, yamlPkg.Name)
	}
	pkg.Archive = yamlPkg.Archive

	for sliceName, yamlSlice := range yamlPkg.Slices {
		match := snameExp.FindStringSubmatch(sliceName)
		if match == nil {
			return nil, fmt.Errorf("invalid slice name %q in %s", sliceName, pkgPath)
		}

		slice := &Slice{
			Package: pkgName,
			Name:    sliceName,
			Scripts: SliceScripts{
				Mutate: yamlSlice.Mutate,
			},
		}

		for _, refName := range yamlSlice.Essential {
			sliceKey, err := ParseSliceKey(refName)
			if err != nil {
				return nil, fmt.Errorf("invalid slice reference %q in %s", refName, pkgPath)
			}
			slice.Essential = append(slice.Essential, sliceKey)
		}

		if len(yamlSlice.Contents) > 0 {
			slice.Contents = make(map[string]PathInfo, len(yamlSlice.Contents))
		}
		for contPath, yamlPath := range yamlSlice.Contents {
			isDir := strings.HasSuffix(contPath, "/")
			comparePath := contPath
			if isDir {
				comparePath = comparePath[:len(comparePath)-1]
			}
			if !path.IsAbs(contPath) || path.Clean(contPath) != comparePath {
				return nil, fmt.Errorf("slice %s_%s has invalid content path: %s", pkgName, sliceName, contPath)
			}
			var kinds = make([]PathKind, 0, 3)
			var info string
			var mode uint
			var mutable bool
			if strings.ContainsAny(contPath, "*?") {
				if yamlPath != nil && (yamlPath.Mode != 0 || yamlPath.Mutable) {
					return nil, fmt.Errorf("invalid slice %s_%s definition for path %s: cannot define details when using wildcards",
						pkgName, sliceName, contPath)
				}
				kinds = append(kinds, GlobPath)
			}
			if yamlPath != nil {
				mode = yamlPath.Mode
				mutable = yamlPath.Mutable
				if yamlPath.Dir {
					if !strings.HasSuffix(contPath, "/") {
						return nil, fmt.Errorf("slice %s_%s content %q must end in / for 'make' to be valid",
							pkgName, sliceName, contPath)
					}
					kinds = append(kinds, DirPath)
				}
				if len(yamlPath.Text) > 0 {
					kinds = append(kinds, TextPath)
					info = yamlPath.Text
				}
				if len(yamlPath.Symlink) > 0 {
					kinds = append(kinds, SymlinkPath)
					info = yamlPath.Symlink
				}
				if len(yamlPath.Copy) > 0 {
					kinds = append(kinds, CopyPath)
					info = yamlPath.Copy
					if info == contPath {
						info = ""
					}
				}
			}
			if len(kinds) == 0 {
				kinds = append(kinds, CopyPath)
			}
			if len(kinds) != 1 {
				list := make([]string, len(kinds))
				for i, s := range kinds {
					list[i] = string(s)
				}
				return nil, fmt.Errorf("conflict in slice %s_%s definition for path %s: %s", pkgName, sliceName, contPath, strings.Join(list, ", "))
			}
			if mutable && kinds[0] != TextPath && (kinds[0] != CopyPath || isDir) {
				return nil, fmt.Errorf("slice %s_%s mutable is not a regular file: %s", pkgName, sliceName, contPath)
			}
			slice.Contents[contPath] = PathInfo{
				Kind:    kinds[0],
				Info:    info,
				Mode:    mode,
				Mutable: mutable,
			}
		}

		pkg.Slices[sliceName] = slice
	}

	return &pkg, err
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

	paths := make(map[string]*Slice)
	for _, new := range selection.Slices {
		for newPath, newInfo := range new.Contents {
			if old, ok := paths[newPath]; ok {
				oldInfo := old.Contents[newPath]
				if newInfo != oldInfo || (newInfo.Kind == CopyPath || newInfo.Kind == GlobPath) && new.Package != old.Package {
					if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
						old, new = new, old
					}
					return nil, fmt.Errorf("slices %s and %s conflict on %s", old, new, newPath)
				}
				continue
			}
			paths[newPath] = new
		}
	}

	return selection, nil
}