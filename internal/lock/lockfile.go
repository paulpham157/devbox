// Copyright 2024 Jetify Inc. and contributors. All rights reserved.
// Use of this source code is governed by the license in the LICENSE file.

package lock

import (
	"context"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pkg/errors"
	"go.jetify.com/devbox/internal/cachehash"
	"go.jetify.com/devbox/internal/devpkg/pkgtype"
	"go.jetify.com/devbox/internal/nix"
	"go.jetify.com/devbox/internal/searcher"
	"go.jetify.com/devbox/nix/flake"
	"go.jetify.com/pkg/runx/impl/types"

	"go.jetify.com/devbox/internal/cuecfg"
)

const lockFileVersion = "1"

// Lightly inspired by package-lock.json
type File struct {
	devboxProject `json:"-"`

	LockFileVersion string `json:"lockfile_version"`

	// Packages is keyed by "canonicalName@version"
	Packages map[string]*Package `json:"packages"`
}

func GetFile(project devboxProject) (*File, error) {
	lockFile := &File{
		devboxProject: project,

		LockFileVersion: lockFileVersion,
		Packages:        map[string]*Package{},
	}
	err := cuecfg.ParseFile(lockFilePath(project.ProjectDir()), lockFile)
	if errors.Is(err, fs.ErrNotExist) {
		return lockFile, nil
	}
	if err != nil {
		return nil, err
	}

	// If the lockfile has legacy StorePath fields, we need to convert them to the new format
	ensurePackagesHaveOutputs(lockFile.Packages)

	return lockFile, nil
}

func (f *File) Add(pkgs ...string) error {
	for _, p := range pkgs {
		if _, err := f.Resolve(p); err != nil {
			return err
		}
	}
	return f.Save()
}

func (f *File) Remove(pkgs ...string) error {
	for _, p := range pkgs {
		delete(f.Packages, p)
	}
	return f.Save()
}

// Resolve updates the in memory copy for performance but does not write to disk
// This avoids writing values that may need to be removed in case of error.
func (f *File) Resolve(pkg string) (*Package, error) {
	entry, hasEntry := f.Packages[pkg]
	if hasEntry && entry.Resolved != "" {
		return f.Packages[pkg], nil
	}

	locked := &Package{}
	_, _, versioned := searcher.ParseVersionedPackage(pkg)
	if pkgtype.IsRunX(pkg) || versioned || pkgtype.IsFlake(pkg) {
		resolved, err := f.FetchResolvedPackage(pkg)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			locked = resolved
		}
	} else if IsLegacyPackage(pkg) {
		// These are legacy packages without a version. Resolve to nixpkgs with
		// whatever hash is in the devbox.json
		locked = &Package{
			Resolved: flake.Installable{
				Ref:      f.Stdenv(),
				AttrPath: pkg,
			}.String(),
			Source: nixpkgSource,
		}
	}
	f.Packages[pkg] = locked

	return f.Packages[pkg], nil
}

// TODO:
// Consider a design change to have the File struct match disk to make this system
// easier to reason about, and have isDirty() compare the in-memory struct to the
// on-disk struct.
//
// Proposal:
// 1. Have an OutputsRaw field and a method called Outputs() to access it.
// Outputs() will check if OutputsRaw is zero-value and fills it in from StorePath.
// 2. Then, in Save(), we can check if OutputsRaw is zero and fill it in prior to writing
// to disk.
func (f *File) Save() error {
	isDirty, err := f.isDirty()
	if err != nil {
		return err
	}
	if !isDirty {
		return nil
	}

	// In SystemInfo, preserve legacy StorePath field and clear out modern Outputs before writing
	// Reason: We want to update `devbox.lock` file only upon a user action
	// such as `devbox update` or `devbox add` or `devbox remove`.
	for pkgName, pkg := range f.Packages {
		for sys, sysInfo := range pkg.Systems {
			if sysInfo.outputIsFromStorePath {
				f.Packages[pkgName].Systems[sys].Outputs = nil
			}
		}
	}
	// We set back the Outputs, if needed, after writing the file, so that future
	// users of the `lock.File` struct will have the correct data.
	defer ensurePackagesHaveOutputs(f.Packages)

	return cuecfg.WriteFile(lockFilePath(f.devboxProject.ProjectDir()), f)
}

func (f *File) UpdateStdenv() error {
	if err := nix.ClearFlakeCache(f.devboxProject.Stdenv()); err != nil {
		return err
	}
	if err := f.Remove(f.devboxProject.Stdenv().String()); err != nil {
		return err
	}
	return f.Add(f.devboxProject.Stdenv().String())
}

// TODO: We should improve a few issues with this function:
// * It shared the same name as Devbox.Stdenv() which is confusing.
// * Since File implements DevboxProject, IDEs really struggle to accurately find call sites.
// (side note, we should remove DevboxProject interface)
// * This function forces a resolution of the stdenv flake which is slow and doesn't give us a
// chance to "prep" the user for some waiting.
// * Should we rename to Nixpkgs() ? Stdenv feels a bit ambiguous.
func (f *File) Stdenv() flake.Ref {
	unlocked := f.devboxProject.Stdenv()
	pkg, err := f.Resolve(unlocked.String())
	if err != nil {
		return unlocked
	}
	ref, err := flake.ParseRef(pkg.Resolved)
	if err != nil {
		return unlocked
	}
	return ref
}

func (f *File) Get(pkg string) *Package {
	entry, hasEntry := f.Packages[pkg]
	if !hasEntry || entry.Resolved == "" {
		return nil
	}
	return entry
}

func (f *File) HasAllowInsecurePackages() bool {
	for _, pkg := range f.Packages {
		if pkg.AllowInsecure {
			return true
		}
	}
	return false
}

// This probably belongs in input.go but can't add it there because it will
// create a circular dependency. We could move Input into own package.
func IsLegacyPackage(pkg string) bool {
	_, _, versioned := searcher.ParseVersionedPackage(pkg)
	return !versioned &&
		!strings.Contains(pkg, ":") &&
		// We don't support absolute paths without "path:" prefix, but adding here
		// just in case we ever do.
		// Landau note: I don't think we should support it, it's hard to read and a
		// bit ambiguous.
		!strings.HasPrefix(pkg, "/")
}

// Tidy ensures that the lockfile has the set of packages corresponding to the devbox.json config.
// It gets rid of older packages that are no longer needed.
func (f *File) Tidy() {
	keep := f.devboxProject.AllPackageNamesIncludingRemovedTriggerPackages()
	keep = append(keep, f.devboxProject.Stdenv().String())
	maps.DeleteFunc(f.Packages, func(key string, pkg *Package) bool {
		return !slices.Contains(keep, key)
	})
}

// IsUpToDateAndInstalled returns true if the lockfile is up to date and the
// local hashes match, which generally indicates all packages are correctly
// installed and print-dev-env has been computed and cached.
func (f *File) IsUpToDateAndInstalled(isFish bool) (bool, error) {
	if dirty, err := f.isDirty(); err != nil {
		return false, err
	} else if dirty {
		return false, nil
	}
	configHash, err := f.devboxProject.ConfigHash()
	if err != nil {
		return false, err
	}
	return isStateUpToDate(UpdateStateHashFileArgs{
		ProjectDir: f.devboxProject.ProjectDir(),
		ConfigHash: configHash,
		IsFish:     isFish,
	})
}

func (f *File) SetOutputsForPackage(pkg string, outputs []Output) error {
	p, err := f.Resolve(pkg)
	if err != nil {
		return err
	}
	if p.Systems == nil {
		p.Systems = map[string]*SystemInfo{}
	}
	if p.Systems[nix.System()] == nil {
		p.Systems[nix.System()] = &SystemInfo{}
	}
	p.Systems[nix.System()].Outputs = outputs
	return f.Save()
}

func (f *File) isDirty() (bool, error) {
	currentHash, err := cachehash.JSON(f)
	if err != nil {
		return false, err
	}
	fileSystemLockFile, err := GetFile(f.devboxProject)
	if err != nil {
		return false, err
	}
	filesystemHash, err := cachehash.JSON(fileSystemLockFile)
	if err != nil {
		return false, err
	}
	return currentHash != filesystemHash, nil
}

func lockFilePath(projectDir string) string {
	return filepath.Join(projectDir, "devbox.lock")
}

func ResolveRunXPackage(ctx context.Context, pkg string) (types.PkgRef, error) {
	ref, err := types.NewPkgRef(strings.TrimPrefix(pkg, pkgtype.RunXPrefix))
	if err != nil {
		return types.PkgRef{}, err
	}

	registry, err := pkgtype.RunXRegistry(ctx)
	if err != nil {
		return types.PkgRef{}, err
	}
	return registry.ResolveVersion(ref)
}
