// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bootstrap

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/pathtools"
)

// This file supports globbing source files in Blueprints files.
//
// The build.ninja file needs to be regenerated any time a file matching the glob is added
// or removed.  The naive solution is to have the build.ninja file depend on all the
// traversed directories, but this will cause the regeneration step to run every time a
// non-matching file is added to a traversed directory, including backup files created by
// editors.
//
// The solution implemented here optimizes out regenerations when the directory modifications
// don't match the glob by having the build.ninja file depend on an intermedate file that
// is only updated when a file matching the glob is added or removed.  The intermediate file
// depends on the traversed directories via a depfile.  The depfile is used to avoid build
// errors if a directory is deleted - a direct dependency on the deleted directory would result
// in a build failure with a "missing and no known rule to make it" error.

var (
	_ = pctx.VariableFunc("globCmd", func(ctx blueprint.VariableFuncContext, config interface{}) (string, error) {
		return filepath.Join(config.(BootstrapConfig).SoongOutDir(), "bpglob"), nil
	})

	// globRule rule traverses directories to produce a list of files that match $glob
	// and writes it to $out if it has changed, and writes the directories to $out.d
	GlobRule = pctx.StaticRule("GlobRule",
		blueprint.RuleParams{
			Command:     "$globCmd -o $out $args",
			CommandDeps: []string{"$globCmd"},
			Description: "glob",

			Restat:  true,
			Deps:    blueprint.DepsGCC,
			Depfile: "$out.d",
		},
		"args")
)

// GlobFileContext is the subset of ModuleContext and SingletonContext needed by GlobFile
type GlobFileContext interface {
	Config() interface{}
	Build(pctx blueprint.PackageContext, params blueprint.BuildParams)
}

// GlobFile creates a rule to write to fileListFile a list of the files that match the specified
// pattern but do not match any of the patterns specified in excludes.  The file will include
// appropriate dependencies to regenerate the file if and only if the list of matching files has
// changed.
func GlobFile(ctx GlobFileContext, pattern string, excludes []string, fileListFile string) {
	args := `-p "` + pattern + `"`
	if len(excludes) > 0 {
		args += " " + joinWithPrefixAndQuote(excludes, "-e ")
	}
	ctx.Build(pctx, blueprint.BuildParams{
		Rule:    GlobRule,
		Outputs: []string{fileListFile},
		Args: map[string]string{
			"args": args,
		},
		Description: "glob " + pattern,
	})
}

// multipleGlobFilesRule creates a rule to write to fileListFile a list of the files that match the specified
// pattern but do not match any of the patterns specified in excludes.  The file will include
// appropriate dependencies to regenerate the file if and only if the list of matching files has
// changed.
func multipleGlobFilesRule(ctx GlobFileContext, fileListFile string, shard int, globs pathtools.MultipleGlobResults) {
	args := strings.Builder{}

	for i, glob := range globs {
		if i != 0 {
			args.WriteString(" ")
		}
		args.WriteString(`-p "`)
		args.WriteString(glob.Pattern)
		args.WriteString(`"`)
		for _, exclude := range glob.Excludes {
			args.WriteString(` -e "`)
			args.WriteString(exclude)
			args.WriteString(`"`)
		}
	}

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:    GlobRule,
		Outputs: []string{fileListFile},
		Args: map[string]string{
			"args": args.String(),
		},
		Description: fmt.Sprintf("regenerate globs shard %d of %d", shard, numGlobBuckets),
	})
}

func joinWithPrefixAndQuote(strs []string, prefix string) string {
	if len(strs) == 0 {
		return ""
	}

	if len(strs) == 1 {
		return prefix + `"` + strs[0] + `"`
	}

	n := len(" ") * (len(strs) - 1)
	for _, s := range strs {
		n += len(prefix) + len(s) + len(`""`)
	}

	ret := make([]byte, 0, n)
	for i, s := range strs {
		if i != 0 {
			ret = append(ret, ' ')
		}
		ret = append(ret, prefix...)
		ret = append(ret, '"')
		ret = append(ret, s...)
		ret = append(ret, '"')
	}
	return string(ret)
}

// GlobSingleton collects any glob patterns that were seen by Context and writes out rules to
// re-evaluate them whenever the contents of the searched directories change, and retrigger the
// primary builder if the results change.
type GlobSingleton struct {
	// A function that returns the glob results of individual glob buckets
	GlobLister func() pathtools.MultipleGlobResults

	// Ninja file that contains instructions for validating the glob list files
	GlobFile string

	// Directory containing the glob list files
	GlobDir string

	// The source directory
	SrcDir string
}

func globBucketName(globDir string, globBucket int) string {
	return filepath.Join(globDir, strconv.Itoa(globBucket))
}

// Returns the directory where glob list files live
func GlobDirectory(buildDir, globListDir string) string {
	return filepath.Join(buildDir, "globs", globListDir)
}

func (s *GlobSingleton) GenerateBuildActions(ctx blueprint.SingletonContext) {
	// Sort the list of globs into buckets.  A hash function is used instead of sharding so that
	// adding a new glob doesn't force rerunning all the buckets by shifting them all by 1.
	globBuckets := make([]pathtools.MultipleGlobResults, numGlobBuckets)
	for _, g := range s.GlobLister() {
		bucket := globToBucket(g)
		globBuckets[bucket] = append(globBuckets[bucket], g)
	}

	for i, globs := range globBuckets {
		fileListFile := globBucketName(s.GlobDir, i)

		// Called from generateGlobNinjaFile.  Write out the file list to disk, and add a ninja
		// rule to run bpglob if any of the dependencies (usually directories that contain
		// globbed files) have changed.  The file list produced by bpglob should match exactly
		// with the file written here so that restat can prevent rerunning the primary builder.
		//
		// We need to write the file list here so that it has an older modified date
		// than the build.ninja (otherwise we'd run the primary builder twice on
		// every new glob)
		//
		// We don't need to write the depfile because we're guaranteed that ninja
		// will run the command at least once (to record it into the ninja_log), so
		// the depfile will be loaded from that execution.
		absoluteFileListFile := joinPath(s.SrcDir, fileListFile)
		err := pathtools.WriteFileIfChanged(absoluteFileListFile, globs.FileList(), 0666)
		if err != nil {
			panic(fmt.Errorf("error writing %s: %s", fileListFile, err))
		}

		// Write out the ninja rule to run bpglob.
		multipleGlobFilesRule(ctx, fileListFile, i, globs)
	}
}

// Writes a .ninja file that contains instructions for regenerating the glob
// files that contain the results of every glob that was run. The list of files
// is available as the result of GlobFileListFiles().
func WriteBuildGlobsNinjaFile(glob *GlobSingleton, config interface{}) error {
	buffer, errs := generateGlobNinjaFile(glob, config)
	if len(errs) > 0 {
		return fatalErrors(errs)
	}

	const outFilePermissions = 0666
	err := ioutil.WriteFile(joinPath(glob.SrcDir, glob.GlobFile), buffer, outFilePermissions)
	if err != nil {
		return fmt.Errorf("error writing %s: %s", glob.GlobFile, err)
	}

	return nil
}

func generateGlobNinjaFile(glob *GlobSingleton, config interface{}) ([]byte, []error) {

	ctx := blueprint.NewContext()
	ctx.RegisterSingletonType("glob", func() blueprint.Singleton {
		return glob
	}, false)

	extraDeps, errs := ctx.ResolveDependencies(config)
	if len(extraDeps) > 0 {
		return nil, []error{fmt.Errorf("shouldn't have extra deps")}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	// PrepareBuildActions() will write $OUTDIR/soong/globs/$m/$i files
	// where $m=bp2build|build and $i=0..numGlobBuckets
	extraDeps, errs = ctx.PrepareBuildActions(config)
	if len(extraDeps) > 0 {
		return nil, []error{fmt.Errorf("shouldn't have extra deps")}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	buf := bytes.NewBuffer(nil)
	err := ctx.WriteBuildFile(buf)
	if err != nil {
		return nil, []error{err}
	}

	return buf.Bytes(), nil
}

// GlobFileListFiles returns the list of files that contain the result of globs
// in the build. It is suitable for inclusion in build.ninja.d (so that
// build.ninja is regenerated if the globs change). The instructions to
// regenerate these files are written by WriteBuildGlobsNinjaFile().
func GlobFileListFiles(globDir string) []string {
	var fileListFiles []string
	for i := 0; i < numGlobBuckets; i++ {
		fileListFile := globBucketName(globDir, i)
		fileListFiles = append(fileListFiles, fileListFile)
	}
	return fileListFiles
}

const numGlobBuckets = 1024

// globToBucket converts a pathtools.GlobResult into a hashed bucket number in the range
// [0, numGlobBuckets).
func globToBucket(g pathtools.GlobResult) int {
	hash := fnv.New32a()
	io.WriteString(hash, g.Pattern)
	for _, e := range g.Excludes {
		io.WriteString(hash, e)
	}
	return int(hash.Sum32() % numGlobBuckets)
}
