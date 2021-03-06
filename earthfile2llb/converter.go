package earthfile2llb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/earthly/earthly/buildcontext"
	"github.com/earthly/earthly/cleanup"
	"github.com/earthly/earthly/debugger/common"
	"github.com/earthly/earthly/dockertar"
	"github.com/earthly/earthly/domain"
	"github.com/earthly/earthly/earthfile2llb/dedup"
	"github.com/earthly/earthly/earthfile2llb/image"
	"github.com/earthly/earthly/earthfile2llb/imr"
	"github.com/earthly/earthly/earthfile2llb/variables"
	"github.com/earthly/earthly/llbutil"
	"github.com/earthly/earthly/llbutil/llbgit"
	"github.com/earthly/earthly/logging"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	solverpb "github.com/moby/buildkit/solver/pb"
	"github.com/pkg/errors"
)

// Converter turns earth commands to buildkit LLB representation.
type Converter struct {
	gitMeta            *buildcontext.GitMetadata
	resolver           *buildcontext.Resolver
	mts                *MultiTargetStates
	directDeps         []*SingleTargetStates
	directDepIndices   []int
	buildContext       llb.State
	cacheContext       llb.State
	varCollection      *variables.Collection
	dockerBuilderFun   DockerBuilderFun
	artifactBuilderFun ArtifactBuilderFun
	cleanCollection    *cleanup.Collection
	nextArgIndex       int
	solveCache         map[string]llb.State
	imageResolveMode   llb.ResolveMode
}

// NewConverter constructs a new converter for a given earth target.
func NewConverter(ctx context.Context, target domain.Target, bc *buildcontext.Data, opt ConvertOpt) (*Converter, error) {
	sts := &SingleTargetStates{
		Target: target,
		TargetInput: dedup.TargetInput{
			TargetCanonical: target.StringCanonical(),
		},
		SideEffectsState: llb.Scratch().Platform(llbutil.TargetPlatform),
		SideEffectsImage: image.NewImage(),
		ArtifactsState:   llb.Scratch().Platform(llbutil.TargetPlatform),
		LocalDirs:        bc.LocalDirs,
		Ongoing:          true,
		Salt:             fmt.Sprintf("%d", rand.Int()),
	}
	mts := &MultiTargetStates{
		FinalStates:   sts,
		VisitedStates: opt.VisitedStates,
	}
	for _, key := range opt.VarCollection.SortedOverridingVariables() {
		ovVar, _, _ := opt.VarCollection.Get(key)
		sts.TargetInput = sts.TargetInput.WithBuildArgInput(ovVar.BuildArgInput(key, ""))
	}
	targetStr := target.String()
	opt.VisitedStates[targetStr] = append(opt.VisitedStates[targetStr], sts)
	return &Converter{
		gitMeta:            bc.GitMetadata,
		resolver:           opt.Resolver,
		imageResolveMode:   opt.ImageResolveMode,
		mts:                mts,
		buildContext:       bc.BuildContext,
		cacheContext:       makeCacheContext(target),
		varCollection:      opt.VarCollection.WithBuiltinBuildArgs(target, bc.GitMetadata),
		dockerBuilderFun:   opt.DockerBuilderFun,
		artifactBuilderFun: opt.ArtifactBuilderFun,
		cleanCollection:    opt.CleanCollection,
		solveCache:         opt.SolveCache,
	}, nil
}

// From applies the earth FROM command.
func (c *Converter) From(ctx context.Context, imageName string, buildArgs []string) error {
	if strings.Contains(imageName, "+") {
		// Target-based FROM.
		return c.fromTarget(ctx, imageName, buildArgs)
	}

	// Docker image based FROM.
	if len(buildArgs) != 0 {
		return errors.New("--build-arg not supported in non-target FROM")
	}
	return c.fromClassical(ctx, imageName)
}

func (c *Converter) fromClassical(ctx context.Context, imageName string) error {
	state, img, newVariables, err := c.internalFromClassical(
		ctx, imageName,
		llb.WithCustomNamef("%sFROM %s", c.vertexPrefix(), imageName))
	if err != nil {
		return err
	}
	c.mts.FinalStates.SideEffectsState = state
	c.mts.FinalStates.SideEffectsImage = img
	c.varCollection = newVariables
	return nil
}

func (c *Converter) fromTarget(ctx context.Context, targetName string, buildArgs []string) error {
	logger := logging.GetLogger(ctx).With("from-target", targetName).With("build-args", buildArgs)
	logger.Info("Applying FROM target")
	depTarget, err := domain.ParseTarget(targetName)
	if err != nil {
		return errors.Wrapf(err, "parse target name %s", targetName)
	}
	mts, err := c.Build(ctx, depTarget.String(), buildArgs)
	if err != nil {
		return errors.Wrapf(err, "apply build %s", depTarget.String())
	}
	if depTarget.IsLocalInternal() {
		depTarget.LocalPath = c.mts.FinalStates.Target.LocalPath
	}
	// Look for the built state in the dep states, after we've built it.
	relevantDepState := mts.FinalStates
	saveImage, ok := relevantDepState.LastSaveImage()
	if !ok {
		return fmt.Errorf(
			"FROM statement: referenced target %s does not contain a SAVE IMAGE statement",
			depTarget.String())
	}

	// Pass on dep state over to this state.
	c.mts.FinalStates.SideEffectsState = saveImage.State
	for dirKey, dirValue := range relevantDepState.LocalDirs {
		c.mts.FinalStates.LocalDirs[dirKey] = dirValue
	}
	for _, kv := range saveImage.Image.Config.Env {
		k, v := variables.ParseKeyValue(kv)
		c.varCollection.AddActive(k, variables.NewConstantEnvVar(v), true)
	}
	c.mts.FinalStates.SideEffectsImage = saveImage.Image.Clone()
	return nil
}

// FromDockerfile applies the earth FROM DOCKERFILE command.
func (c *Converter) FromDockerfile(ctx context.Context, contextPath string, dfPath string, dfTarget string, buildArgs []string) error {
	if dfPath != "" {
		// TODO: It's not yet very clear what -f should do. Should it be referencing a Dockerfile
		//       from the build context or the build environment?
		//       Build environment is likely better as it gives maximum flexibility to do
		//       anything.
		return errors.New("FROM DOCKERFILE -f not yet supported")
	}
	var buildContext llb.State
	if strings.Contains(contextPath, "+") {
		// The Dockerfile and build context are from a target's artifact.
		contextArtifact, err := domain.ParseArtifact(contextPath)
		if err != nil {
			return errors.Wrapf(err, "parse artifact %s", contextPath)
		}
		// TODO: The build args are used for both the artifact and the Dockerfile. This could be
		//       confusing to the user.
		mts, err := c.Build(ctx, contextArtifact.Target.String(), buildArgs)
		if err != nil {
			return err
		}
		pathArtifact, err := c.solveArtifact(ctx, mts, contextArtifact)
		if err != nil {
			return err
		}
		dfPath = filepath.Join(pathArtifact, "Dockerfile")
		buildContext = llb.Scratch().Platform(llbutil.TargetPlatform)
		buildContext = llbutil.CopyOp(
			mts.FinalStates.ArtifactsState, []string{contextArtifact.Artifact},
			buildContext, "/", true, true, "",
			llb.WithCustomNamef(
				"[internal] FROM DOCKERFILE (copy build context from) %s%s",
				joinWrap(buildArgs, "(", " ", ") "), contextArtifact.String()))
	} else {
		// The Dockerfile and build context are from the host.
		if contextPath != "." &&
			!strings.HasPrefix(contextPath, "./") &&
			!strings.HasPrefix(contextPath, "../") &&
			!strings.HasPrefix(contextPath, "/") {
			contextPath = fmt.Sprintf("./%s", contextPath)
		}
		dockerfileMetaTarget := domain.Target{
			Target:    buildcontext.DockerfileMetaTarget,
			LocalPath: contextPath,
		}
		dockerfileMetaTarget, err := domain.JoinTargets(c.mts.FinalTarget(), dockerfileMetaTarget)
		if err != nil {
			return errors.Wrap(err, "join targets")
		}
		data, err := c.resolver.Resolve(ctx, dockerfileMetaTarget)
		if err != nil {
			return errors.Wrap(err, "resolve build context for dockerfile")
		}
		for ldk, ld := range data.LocalDirs {
			c.mts.FinalStates.LocalDirs[ldk] = ld
		}
		dfPath = data.BuildFilePath
		buildContext = data.BuildContext
	}
	dfData, err := ioutil.ReadFile(dfPath)
	if err != nil {
		return errors.Wrapf(err, "read file %s", dfPath)
	}
	newVarCollection, err := c.varCollection.WithParseBuildArgs(
		buildArgs, c.processNonConstantBuildArgFunc(ctx))
	if err != nil {
		return err
	}
	caps := solverpb.Caps.CapSet(solverpb.Caps.All())
	state, dfImg, err := dockerfile2llb.Dockerfile2LLB(ctx, dfData, dockerfile2llb.ConvertOpt{
		BuildContext:     &buildContext,
		ContextLocalName: c.mts.FinalTarget().String(),
		MetaResolver:     imr.Default(),
		ImageResolveMode: c.imageResolveMode,
		Target:           dfTarget,
		TargetPlatform:   &llbutil.TargetPlatform,
		LLBCaps:          &caps,
		BuildArgs:        newVarCollection.AsMap(),
		Excludes:         nil, // TODO: Need to process this correctly.
	})
	if err != nil {
		return errors.Wrapf(err, "dockerfile2llb %s", dfPath)
	}
	// Convert dockerfile2llb image into earthfile2llb image via JSON.
	imgDt, err := json.Marshal(dfImg)
	if err != nil {
		return errors.Wrap(err, "marshal dockerfile image")
	}
	var img image.Image
	err = json.Unmarshal(imgDt, &img)
	if err != nil {
		return errors.Wrap(err, "unmarshal dockerfile image")
	}
	state2, img2, newVarCollection := c.applyFromImage(*state, &img)
	c.mts.FinalStates.SideEffectsState = state2
	c.mts.FinalStates.SideEffectsImage = img2
	c.varCollection = newVarCollection
	return nil
}

// CopyArtifact applies the earth COPY artifact command.
func (c *Converter) CopyArtifact(ctx context.Context, artifactName string, dest string, buildArgs []string, isDir bool, chown string) error {
	logging.GetLogger(ctx).
		With("srcArtifact", artifactName).
		With("dest", dest).
		With("build-args", buildArgs).
		With("dir", isDir).
		With("chown", chown).
		Info("Applying COPY (artifact)")
	artifact, err := domain.ParseArtifact(artifactName)
	if err != nil {
		return errors.Wrapf(err, "parse artifact name %s", artifactName)
	}
	mts, err := c.Build(ctx, artifact.Target.String(), buildArgs)
	if err != nil {
		return errors.Wrapf(err, "apply build %s", artifact.Target.String())
	}
	if artifact.Target.IsLocalInternal() {
		artifact.Target.LocalPath = c.mts.FinalStates.Target.LocalPath
	}
	// Grab the artifacts state in the dep states, after we've built it.
	relevantDepState := mts.FinalStates
	// Copy.
	c.mts.FinalStates.SideEffectsState = llbutil.CopyOp(
		relevantDepState.ArtifactsState, []string{artifact.Artifact},
		c.mts.FinalStates.SideEffectsState, dest, true, isDir, chown,
		llb.WithCustomNamef(
			"%sCOPY %s%s%s %s",
			c.vertexPrefix(),
			strIf(isDir, "--dir "),
			joinWrap(buildArgs, "(", " ", ") "),
			artifact.String(),
			dest))
	return nil
}

// CopyClassical applies the earth COPY command, with classical args.
func (c *Converter) CopyClassical(ctx context.Context, srcs []string, dest string, isDir bool, chown string) {
	logging.GetLogger(ctx).
		With("srcs", srcs).
		With("dest", dest).
		With("dir", isDir).
		With("chown", chown).
		Info("Applying COPY (classical)")
	c.mts.FinalStates.SideEffectsState = llbutil.CopyOp(
		c.buildContext, srcs, c.mts.FinalStates.SideEffectsState, dest, true, isDir, chown,
		llb.WithCustomNamef(
			"%sCOPY %s%s %s",
			c.vertexPrefix(),
			strIf(isDir, "--dir "),
			strings.Join(srcs, " "),
			dest))
}

// Run applies the earth RUN command.
func (c *Converter) Run(ctx context.Context, args []string, mounts []string, secretKeyValues []string, privileged bool, withEntrypoint bool, withDocker bool, isWithShell bool, pushFlag bool, withSSH bool) error {
	if withDocker {
		fmt.Printf("Warning: RUN --with-docker is deprecated. Use WITH DOCKER ... RUN ... END instead\n")
	}
	logging.GetLogger(ctx).
		With("args", args).
		With("mounts", mounts).
		With("secrets", secretKeyValues).
		With("privileged", privileged).
		With("withEntrypoint", withEntrypoint).
		With("withDocker", withDocker).
		With("push", pushFlag).
		With("withSSH", withSSH).
		Info("Applying RUN")
	var opts []llb.RunOption
	mountRunOpts, err := parseMounts(mounts, c.mts.FinalStates.Target, c.mts.FinalStates.TargetInput, c.cacheContext)
	if err != nil {
		return errors.Wrap(err, "parse mounts")
	}
	opts = append(opts, mountRunOpts...)

	finalArgs := args
	if withEntrypoint {
		if len(args) == 0 {
			// No args provided. Use the image's CMD.
			args := make([]string, len(c.mts.FinalStates.SideEffectsImage.Config.Cmd))
			copy(args, c.mts.FinalStates.SideEffectsImage.Config.Cmd)
		}
		finalArgs = append(c.mts.FinalStates.SideEffectsImage.Config.Entrypoint, args...)
		isWithShell = false // Don't use shell when --entrypoint is passed.
	}
	if privileged {
		opts = append(opts, llb.Security(llb.SecurityModeInsecure))
	}
	runStr := fmt.Sprintf(
		"RUN %s%s%s%s%s",
		strIf(privileged, "--privileged "),
		strIf(withDocker, "--with-docker "),
		strIf(withEntrypoint, "--entrypoint "),
		strIf(pushFlag, "--push "),
		strings.Join(finalArgs, " "))
	shellWrap := withShellAndEnvVars
	if withDocker {
		shellWrap = withDockerdWrapOld
	}
	opts = append(opts, llb.WithCustomNamef("%s%s", c.vertexPrefix(), runStr))
	return c.internalRun(ctx, finalArgs, secretKeyValues, isWithShell, shellWrap, pushFlag, withSSH, runStr, opts...)
}

// SaveArtifact applies the earth SAVE ARTIFACT command.
func (c *Converter) SaveArtifact(ctx context.Context, saveFrom string, saveTo string, saveAsLocalTo string) error {
	logging.GetLogger(ctx).
		With("saveFrom", saveFrom).
		With("saveTo", saveTo).
		With("saveAsLocalTo", saveAsLocalTo).
		Info("Applying SAVE ARTIFACT")
	saveToAdjusted := saveTo
	if saveTo == "" || saveTo == "." || strings.HasSuffix(saveTo, "/") {
		absSaveFrom, err := llbutil.Abs(ctx, c.mts.FinalStates.SideEffectsState, saveFrom)
		if err != nil {
			return err
		}
		saveFromRelative := path.Join(".", absSaveFrom)
		saveToAdjusted = path.Join(saveTo, path.Base(saveFromRelative))
	}
	saveToD, saveToF := splitWildcards(saveToAdjusted)
	var artifactPath string
	if saveToF == "" {
		artifactPath = saveToAdjusted
	} else {
		saveToAdjusted = fmt.Sprintf("%s/", saveToD)
		artifactPath = path.Join(saveToAdjusted, saveToF)
	}
	artifact := domain.Artifact{
		Target:   c.mts.FinalStates.Target,
		Artifact: artifactPath,
	}
	c.mts.FinalStates.ArtifactsState = llbutil.CopyOp(
		c.mts.FinalStates.SideEffectsState, []string{saveFrom}, c.mts.FinalStates.ArtifactsState,
		saveToAdjusted, true, true, "",
		llb.WithCustomNamef(
			"%sSAVE ARTIFACT %s %s", c.vertexPrefix(), saveFrom, artifact.String()))
	if saveAsLocalTo != "" {
		separateArtifactsState := llb.Scratch().Platform(llbutil.TargetPlatform)
		separateArtifactsState = llbutil.CopyOp(
			c.mts.FinalStates.SideEffectsState, []string{saveFrom}, separateArtifactsState,
			saveToAdjusted, true, false, "",
			llb.WithCustomNamef(
				"%sSAVE ARTIFACT %s %s AS LOCAL %s",
				c.vertexPrefix(), saveFrom, artifact.String(), saveAsLocalTo))
		c.mts.FinalStates.SeparateArtifactsState = append(c.mts.FinalStates.SeparateArtifactsState, separateArtifactsState)
		c.mts.FinalStates.SaveLocals = append(c.mts.FinalStates.SaveLocals, SaveLocal{
			DestPath:     saveAsLocalTo,
			ArtifactPath: artifactPath,
			Index:        len(c.mts.FinalStates.SeparateArtifactsState) - 1,
		})
	}
	return nil
}

// SaveImage applies the earth SAVE IMAGE command.
func (c *Converter) SaveImage(ctx context.Context, imageNames []string, pushImages bool) {
	logging.GetLogger(ctx).With("image", imageNames).With("push", pushImages).Info("Applying SAVE IMAGE")
	if len(imageNames) == 0 {
		// Use an empty image name if none provided. This will not be exported
		// as docker image, but will allow for importing / referencing within
		// earthfiles.
		imageNames = []string{""}
	}
	for _, imageName := range imageNames {
		c.mts.FinalStates.SaveImages = append(c.mts.FinalStates.SaveImages, SaveImage{
			State:     c.mts.FinalStates.SideEffectsState,
			Image:     c.mts.FinalStates.SideEffectsImage.Clone(),
			DockerTag: imageName,
			Push:      pushImages,
		})
	}
}

// Build applies the earth BUILD command.
func (c *Converter) Build(ctx context.Context, fullTargetName string, buildArgs []string) (*MultiTargetStates, error) {
	logging.GetLogger(ctx).
		With("full-target-name", fullTargetName).
		With("build-args", buildArgs).
		Info("Applying BUILD")

	relTarget, err := domain.ParseTarget(fullTargetName)
	if err != nil {
		return nil, errors.Wrapf(err, "earth target parse %s", fullTargetName)
	}
	target, err := domain.JoinTargets(c.mts.FinalStates.Target, relTarget)
	if err != nil {
		return nil, errors.Wrap(err, "join targets")
	}
	newVarCollection := c.varCollection
	if relTarget.IsExternal() {
		// Don't allow transitive overriding variables to cross project boundaries.
		newVarCollection = variables.NewCollection()
	}
	newVarCollection, err = newVarCollection.WithParseBuildArgs(
		buildArgs, c.processNonConstantBuildArgFunc(ctx))
	if err != nil {
		return nil, errors.Wrap(err, "parse build args")
	}
	// Recursion.
	mts, err := Earthfile2LLB(
		ctx, target, ConvertOpt{
			Resolver:         c.resolver,
			ImageResolveMode: c.imageResolveMode,
			DockerBuilderFun: c.dockerBuilderFun,
			CleanCollection:  c.cleanCollection,
			VisitedStates:    c.mts.VisitedStates,
			VarCollection:    newVarCollection,
			SolveCache:       c.solveCache,
		})
	if err != nil {
		return nil, errors.Wrapf(err, "earthfile2llb for %s", fullTargetName)
	}
	c.directDeps = append(c.directDeps, mts.FinalStates)
	return mts, nil
}

// Workdir applies the WORKDIR command.
func (c *Converter) Workdir(ctx context.Context, workdirPath string) {
	logging.GetLogger(ctx).With("workdir", workdirPath).Info("Applying WORKDIR")
	c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.Dir(workdirPath)
	workdirAbs := workdirPath
	if !path.IsAbs(workdirAbs) {
		workdirAbs = path.Join("/", c.mts.FinalStates.SideEffectsImage.Config.WorkingDir, workdirAbs)
	}
	c.mts.FinalStates.SideEffectsImage.Config.WorkingDir = workdirAbs
	if workdirAbs != "/" {
		// Mkdir.
		mkdirOpts := []llb.MkdirOption{
			llb.WithParents(true),
		}
		if c.mts.FinalStates.SideEffectsImage.Config.User != "" {
			mkdirOpts = append(mkdirOpts, llb.WithUser(c.mts.FinalStates.SideEffectsImage.Config.User))
		}
		opts := []llb.ConstraintsOpt{
			llb.WithCustomNamef("%sWORKDIR %s", c.vertexPrefix(), workdirPath),
		}
		c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.File(
			llb.Mkdir(workdirAbs, 0755, mkdirOpts...), opts...)
	}
}

// User applies the USER command.
func (c *Converter) User(ctx context.Context, user string) {
	logging.GetLogger(ctx).With("user", user).Info("Applying USER")
	c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.User(user)
	c.mts.FinalStates.SideEffectsImage.Config.User = user
}

// Cmd applies the CMD command.
func (c *Converter) Cmd(ctx context.Context, cmdArgs []string, isWithShell bool) {
	logging.GetLogger(ctx).With("cmd", cmdArgs).Info("Applying CMD")
	c.mts.FinalStates.SideEffectsImage.Config.Cmd = withShell(cmdArgs, isWithShell)
}

// Entrypoint applies the ENTRYPOINT command.
func (c *Converter) Entrypoint(ctx context.Context, entrypointArgs []string, isWithShell bool) {
	logging.GetLogger(ctx).With("entrypoint", entrypointArgs).Info("Applying ENTRYPOINT")
	c.mts.FinalStates.SideEffectsImage.Config.Entrypoint = withShell(entrypointArgs, isWithShell)
}

// Expose applies the EXPOSE command.
func (c *Converter) Expose(ctx context.Context, ports []string) {
	logging.GetLogger(ctx).With("ports", ports).Info("Applying EXPOSE")
	for _, port := range ports {
		c.mts.FinalStates.SideEffectsImage.Config.ExposedPorts[port] = struct{}{}
	}
}

// Volume applies the VOLUME command.
func (c *Converter) Volume(ctx context.Context, volumes []string) {
	logging.GetLogger(ctx).With("volumes", volumes).Info("Applying VOLUME")
	for _, volume := range volumes {
		c.mts.FinalStates.SideEffectsImage.Config.Volumes[volume] = struct{}{}
	}
}

// Env applies the ENV command.
func (c *Converter) Env(ctx context.Context, envKey string, envValue string) {
	logging.GetLogger(ctx).With("env-key", envKey).With("env-value", envValue).Info("Applying ENV")
	c.varCollection.AddActive(envKey, variables.NewConstantEnvVar(envValue), true)
	c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.AddEnv(envKey, envValue)
	c.mts.FinalStates.SideEffectsImage.Config.Env = variables.AddEnv(
		c.mts.FinalStates.SideEffectsImage.Config.Env, envKey, envValue)
}

// Arg applies the ARG command.
func (c *Converter) Arg(ctx context.Context, argKey string, defaultArgValue string) {
	logging.GetLogger(ctx).With("arg-key", argKey).With("arg-value", defaultArgValue).Info("Applying ARG")
	effective := c.varCollection.AddActive(argKey, variables.NewConstant(defaultArgValue), false)
	c.mts.FinalStates.TargetInput = c.mts.FinalStates.TargetInput.WithBuildArgInput(
		effective.BuildArgInput(argKey, defaultArgValue))
}

// Label applies the LABEL command.
func (c *Converter) Label(ctx context.Context, labels map[string]string) {
	logging.GetLogger(ctx).With("labels", labels).Info("Applying LABEL")
	for key, value := range labels {
		c.mts.FinalStates.SideEffectsImage.Config.Labels[key] = value
	}
}

// GitClone applies the GIT CLONE command.
func (c *Converter) GitClone(ctx context.Context, gitURL string, branch string, dest string) error {
	logging.GetLogger(ctx).With("git-url", gitURL).With("branch", branch).Info("Applying GIT CLONE")
	gitOpts := []llb.GitOption{
		llb.WithCustomNamef(
			"%sGIT CLONE (--branch %s) %s", c.vertexPrefixWithURL(gitURL), branch, gitURL),
		llb.KeepGitDir(),
	}
	gitState := llbgit.Git(gitURL, branch, gitOpts...)
	c.mts.FinalStates.SideEffectsState = llbutil.CopyOp(
		gitState, []string{"."}, c.mts.FinalStates.SideEffectsState, dest, false, false, "",
		llb.WithCustomNamef(
			"%sCOPY GIT CLONE (--branch %s) %s TO %s", c.vertexPrefix(),
			branch, gitURL, dest))
	return nil
}

// WithDockerRun applies an entire WITH DOCKER ... RUN ... END clause.
func (c *Converter) WithDockerRun(ctx context.Context, args []string, opt WithDockerOpt) error {
	wdr := &withDockerRun{
		c: c,
	}
	return wdr.Run(ctx, args, opt)
}

// DockerLoadOld applies the DOCKER LOAD command (outside of WITH DOCKER).
func (c *Converter) DockerLoadOld(ctx context.Context, targetName string, dockerTag string, buildArgs []string) error {
	fmt.Printf("Warning: DOCKER LOAD outside of WITH DOCKER is deprecated\n")
	logging.GetLogger(ctx).With("target-name", targetName).With("dockerTag", dockerTag).Info("Applying DOCKER LOAD")
	depTarget, err := domain.ParseTarget(targetName)
	if err != nil {
		return errors.Wrapf(err, "parse target %s", targetName)
	}
	mts, err := c.Build(ctx, depTarget.String(), buildArgs)
	if err != nil {
		return err
	}
	err = c.solveAndLoadOld(
		ctx, mts, depTarget.String(), dockerTag,
		llb.WithCustomNamef(
			"%sDOCKER LOAD %s %s", c.vertexPrefix(), depTarget.String(), dockerTag))
	if err != nil {
		return err
	}
	return nil
}

// DockerPullOld applies the DOCKER PULL command (outside of WITH DOCKER).
func (c *Converter) DockerPullOld(ctx context.Context, dockerTag string) error {
	fmt.Printf("Warning: DOCKER PULL outside of WITH DOCKER is deprecated\n")
	logging.GetLogger(ctx).With("dockerTag", dockerTag).Info("Applying DOCKER PULL")
	state, image, _, err := c.internalFromClassical(
		ctx, dockerTag,
		llb.WithCustomNamef("%sDOCKER PULL %s", c.vertexPrefix(), dockerTag),
	)
	if err != nil {
		return err
	}
	mts := &MultiTargetStates{
		FinalStates: &SingleTargetStates{
			SideEffectsState: state,
			SideEffectsImage: image,
			SaveImages: []SaveImage{
				{
					State:     state,
					Image:     image,
					DockerTag: dockerTag,
				},
			},
		},
	}
	err = c.solveAndLoadOld(
		ctx, mts, dockerTag, dockerTag,
		llb.WithCustomNamef("%sDOCKER LOAD (PULL %s)", c.vertexPrefix(), dockerTag))
	if err != nil {
		return err
	}
	return nil
}

// Healthcheck applies the HEALTHCHECK command.
func (c *Converter) Healthcheck(ctx context.Context, isNone bool, cmdArgs []string, interval time.Duration, timeout time.Duration, startPeriod time.Duration, retries int) {
	logging.GetLogger(ctx).
		With("isNone", isNone).
		With("cmdArgs", cmdArgs).
		With("interval", interval).
		With("timeout", timeout).
		With("startPeriod", startPeriod).
		With("retries", retries).
		Info("Applying HEALTHCHECK")
	hc := &dockerfile2llb.HealthConfig{}
	if isNone {
		hc.Test = []string{"NONE"}
	} else {
		// TODO: Should support also CMD without shell (exec form).
		//       See https://github.com/moby/buildkit/blob/master/frontend/dockerfile/dockerfile2llb/image.go#L18
		hc.Test = append([]string{"CMD-SHELL", strings.Join(cmdArgs, " ")})
		hc.Interval = interval
		hc.Timeout = timeout
		hc.StartPeriod = startPeriod
		hc.Retries = retries
	}
	c.mts.FinalStates.SideEffectsImage.Config.Healthcheck = hc
}

// FinalizeStates returns the LLB states.
func (c *Converter) FinalizeStates() *MultiTargetStates {
	// Create an artificial bond to depStates so that side-effects of deps are built automatically.
	for _, depStates := range c.directDeps {
		c.mts.FinalStates.SideEffectsState = withDependency(
			c.mts.FinalStates.SideEffectsState,
			c.mts.FinalStates.Target,
			depStates.SideEffectsState,
			depStates.Target)
	}

	c.mts.FinalStates.Ongoing = false
	return c.mts
}

func (c *Converter) internalRun(ctx context.Context, args []string, secretKeyValues []string, isWithShell bool, shellWrap shellWrapFun, pushFlag bool, withSSH bool, commandStr string, opts ...llb.RunOption) error {
	finalOpts := opts
	var extraEnvVars []string
	// Secrets.
	for _, secretKeyValue := range secretKeyValues {
		parts := strings.SplitN(secretKeyValue, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("Invalid secret definition %s", secretKeyValue)
		}
		if !strings.HasPrefix(parts[1], "+secrets/") {
			return fmt.Errorf("Secret definition %s not supported. Must start with +secrets/", secretKeyValue)
		}
		envVar := parts[0]
		secretID := strings.TrimPrefix(parts[1], "+secrets/")
		secretPath := path.Join("/run/secrets", secretID)
		secretOpts := []llb.SecretOption{
			llb.SecretID(secretID),
			// TODO: Perhaps this should just default to the current user automatically from
			//       buildkit side. Then we wouldn't need to open this up to everyone.
			llb.SecretFileOpt(0, 0, 0444),
		}
		finalOpts = append(finalOpts, llb.AddSecret(secretPath, secretOpts...))
		// TODO: The use of cat here might not be portable.
		extraEnvVars = append(extraEnvVars, fmt.Sprintf("%s=\"$(cat %s)\"", envVar, secretPath))
	}
	// Build args.
	for _, buildArgName := range c.varCollection.SortedActiveVariables() {
		ba, _, _ := c.varCollection.Get(buildArgName)
		if ba.IsEnvVar() {
			continue
		}
		if ba.IsConstant() {
			extraEnvVars = append(extraEnvVars, fmt.Sprintf("%s=\"%s\"", buildArgName, ba.ConstantValue()))
		} else {
			buildArgPath := path.Join("/run/buildargs", buildArgName)
			finalOpts = append(finalOpts, llb.AddMount(buildArgPath, ba.VariableState(), llb.SourcePath(buildArgPath)))
			// TODO: The use of cat here might not be portable.
			extraEnvVars = append(extraEnvVars, fmt.Sprintf("%s=\"$(cat %s)\"", buildArgName, buildArgPath))
		}
	}
	// Debugger.
	secretOpts := []llb.SecretOption{
		llb.SecretID(common.DebuggerSettingsSecretsKey),
		llb.SecretFileOpt(0, 0, 0444),
	}
	debuggerSecretMount := llb.AddSecret(
		fmt.Sprintf("/run/secrets/%s", common.DebuggerSettingsSecretsKey), secretOpts...)
	debuggerMount := llb.AddMount(debuggerPath, llb.Scratch(),
		llb.HostBind(), llb.SourcePath("/usr/bin/earth_debugger"))
	runEarthlyMount := llb.AddMount("/run/earthly", llb.Scratch(),
		llb.HostBind(), llb.SourcePath("/run/earthly"))
	finalOpts = append(finalOpts, debuggerSecretMount, debuggerMount, runEarthlyMount)
	if withSSH {
		finalOpts = append(finalOpts, llb.AddSSHSocket())
	}
	// Shell and debugger wrap.
	finalArgs := shellWrap(args, extraEnvVars, isWithShell, true)
	finalOpts = append(finalOpts, llb.Args(finalArgs))
	if pushFlag {
		// For push-flagged commands, make sure they run every time - don't use cache.
		finalOpts = append(finalOpts, llb.IgnoreCache)
		if !c.mts.FinalStates.RunPush.Initialized {
			// If this is the first push-flagged command, initialize the state with the latest
			// side-effects state.
			c.mts.FinalStates.RunPush.State = c.mts.FinalStates.SideEffectsState
			c.mts.FinalStates.RunPush.Initialized = true
		}
		// Don't run on SideEffectsState. We want push-flagged commands to be executed only
		// *after* the build. Save this for later.
		c.mts.FinalStates.RunPush.State = c.mts.FinalStates.RunPush.State.Run(finalOpts...).Root()
		c.mts.FinalStates.RunPush.CommandStrs = append(
			c.mts.FinalStates.RunPush.CommandStrs, commandStr)
	} else {
		c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.Run(finalOpts...).Root()
	}
	return nil
}

func (c *Converter) solveAndLoadOld(ctx context.Context, mts *MultiTargetStates, opName string, dockerTag string, opts ...llb.RunOption) error {
	// Use a builder to create docker .tar file, mount it via a local build context,
	// then docker load it within the current side effects state.
	outDir, err := ioutil.TempDir("/tmp", "earthly-docker-load")
	if err != nil {
		return errors.Wrap(err, "mk temp dir for docker load")
	}
	c.cleanCollection.Add(func() error {
		return os.RemoveAll(outDir)
	})
	outFile := path.Join(outDir, "image.tar")
	err = c.dockerBuilderFun(ctx, mts, dockerTag, outFile)
	if err != nil {
		return errors.Wrapf(err, "build target %s for docker load", opName)
	}
	dockerImageID, err := dockertar.GetID(outFile)
	if err != nil {
		return errors.Wrap(err, "inspect docker tar after build")
	}
	// Use the docker image ID + dockerTag as sessionID. This will cause
	// buildkit to use cache when these are the same as before (eg a docker image
	// that is identical as before).
	sessionIDKey := fmt.Sprintf("%s-%s", dockerTag, dockerImageID)
	sha256SessionIDKey := sha256.Sum256([]byte(sessionIDKey))
	sessionID := hex.EncodeToString(sha256SessionIDKey[:])
	// Add the tar to the local context.
	tarContext := llb.Local(
		opName,
		llb.SharedKeyHint(opName),
		llb.SessionID(sessionID),
		llb.Platform(llbutil.TargetPlatform),
		llb.WithCustomNamef("[internal] docker tar context %s %s", opName, sessionID),
	)
	c.mts.FinalStates.LocalDirs[opName] = outDir

	c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.File(
		llb.Mkdir("/var/lib/docker", 0755, llb.WithParents(true)),
		llb.WithCustomNamef("[internal] mkdir /var/lib/docker"),
	)
	loadOpts := []llb.RunOption{
		llb.Args(
			withDockerdWrapOld(
				[]string{"docker", "load", "</src/image.tar"}, []string{}, true, false)),
		llb.AddMount("/src", tarContext, llb.Readonly),
		llb.Dir("/src"),
		llb.Security(llb.SecurityModeInsecure),
	}
	loadOpts = append(loadOpts, opts...)
	loadOp := c.mts.FinalStates.SideEffectsState.Run(loadOpts...)
	c.mts.FinalStates.SideEffectsState = loadOp.AddMount(
		"/var/lib/docker", c.mts.FinalStates.SideEffectsState,
		llb.SourcePath("/var/lib/docker"))
	return nil
}

func (c *Converter) solveArtifact(ctx context.Context, mts *MultiTargetStates, artifact domain.Artifact) (string, error) {
	outDir, err := ioutil.TempDir("/tmp", "earthly-solve-artifact")
	if err != nil {
		return "", errors.Wrap(err, "mk temp dir for solve artifact")
	}
	c.cleanCollection.Add(func() error {
		return os.RemoveAll(outDir)
	})
	err = c.artifactBuilderFun(ctx, mts, artifact, fmt.Sprintf("%s/", outDir))
	if err != nil {
		return "", errors.Wrapf(err, "build artifact %s", artifact.String())
	}
	return outDir, nil
}

func (c *Converter) internalFromClassical(ctx context.Context, imageName string, opts ...llb.ImageOption) (llb.State, *image.Image, *variables.Collection, error) {
	logging.GetLogger(ctx).With("image", imageName).Info("Applying FROM")
	if imageName == "scratch" {
		// FROM scratch
		return llb.Scratch().Platform(llbutil.TargetPlatform), image.NewImage(),
			c.varCollection.WithResetEnvVars(), nil
	}
	ref, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return llb.State{}, nil, nil, errors.Wrapf(err, "parse normalized named %s", imageName)
	}
	baseImageName := reference.TagNameOnly(ref).String()
	metaResolver := imr.Default()
	dgst, dt, err := metaResolver.ResolveImageConfig(
		ctx, baseImageName,
		llb.ResolveImageConfigOpt{
			Platform:    &llbutil.TargetPlatform,
			ResolveMode: c.imageResolveMode.String(),
			LogName:     fmt.Sprintf("%sLoad metadata", c.imageVertexPrefix(imageName)),
		})
	if err != nil {
		return llb.State{}, nil, nil, errors.Wrapf(err, "resolve image config for %s", imageName)
	}
	var img image.Image
	err = json.Unmarshal(dt, &img)
	if err != nil {
		return llb.State{}, nil, nil, errors.Wrapf(err, "unmarshal image config for %s", imageName)
	}
	if dgst != "" {
		ref, err = reference.WithDigest(ref, dgst)
		if err != nil {
			return llb.State{}, nil, nil, errors.Wrapf(err, "reference add digest %v for %s", dgst, imageName)
		}
	}
	allOpts := append(opts, llb.Platform(llbutil.TargetPlatform), c.imageResolveMode)
	state := llb.Image(ref.String(), allOpts...)
	state, img2, newVarCollection := c.applyFromImage(state, &img)
	return state, img2, newVarCollection, nil
}

func (c *Converter) applyFromImage(state llb.State, img *image.Image) (llb.State, *image.Image, *variables.Collection) {
	// Reset variables.
	newVarCollection := c.varCollection.WithResetEnvVars()
	for _, envVar := range img.Config.Env {
		k, v := variables.ParseKeyValue(envVar)
		newVarCollection.AddActive(k, variables.NewConstantEnvVar(v), true)
		state = state.AddEnv(k, v)
	}
	// Init config maps if not already initialized.
	if img.Config.ExposedPorts == nil {
		img.Config.ExposedPorts = make(map[string]struct{})
	}
	if img.Config.Labels == nil {
		img.Config.Labels = make(map[string]string)
	}
	if img.Config.Volumes == nil {
		img.Config.Volumes = make(map[string]struct{})
	}
	if img.Config.WorkingDir != "" {
		state = state.Dir(img.Config.WorkingDir)
	}
	if img.Config.User != "" {
		state = state.User(img.Config.User)
	}
	// No need to apply entrypoint, cmd, volumes and others.
	// The fact that they exist in the image configuration is enough.
	// TODO: Apply any other settings? Shell?
	return state, img, newVarCollection
}

// ExpandArgs expands args in the provided word.
func (c *Converter) ExpandArgs(word string) string {
	return c.varCollection.Expand(word)
}

func (c *Converter) processNonConstantBuildArgFunc(ctx context.Context) variables.ProcessNonConstantVariableFunc {
	return func(name string, expression string) (llb.State, dedup.TargetInput, int, error) {
		// Run the expression on the side effects state.
		srcBuildArgDir := "/run/buildargs-src"
		srcBuildArgPath := path.Join(srcBuildArgDir, name)
		c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.File(
			llb.Mkdir(srcBuildArgDir, 0755, llb.WithParents(true)),
			llb.WithCustomNamef("[internal] mkdir %s", srcBuildArgDir))
		buildArgPath := path.Join("/run/buildargs", name)
		args := strings.Split(fmt.Sprintf("echo \"%s\" >%s", expression, srcBuildArgPath), " ")
		err := c.internalRun(
			ctx, args, []string{}, true, withShellAndEnvVars, false, false, expression,
			llb.WithCustomNamef("%sRUN %s", c.vertexPrefix(), expression))
		if err != nil {
			return llb.State{}, dedup.TargetInput{}, 0, errors.Wrapf(err, "run %v", expression)
		}
		// Copy the result of the expression into a separate, isolated state.
		buildArgState := llb.Scratch().Platform(llbutil.TargetPlatform)
		buildArgState = llbutil.CopyOp(
			c.mts.FinalStates.SideEffectsState, []string{srcBuildArgPath},
			buildArgState, buildArgPath, false, false, "",
			llb.WithCustomNamef("[internal] copy buildarg %s", name))
		// Store the state with the expression result for later use.
		argIndex := c.nextArgIndex
		c.nextArgIndex++
		// Remove intermediary file from side effects state.
		c.mts.FinalStates.SideEffectsState = c.mts.FinalStates.SideEffectsState.File(
			llb.Rm(srcBuildArgPath, llb.WithAllowNotFound(true)),
			llb.WithCustomNamef("[internal] rm %s", srcBuildArgPath))

		return buildArgState, c.mts.FinalStates.TargetInput, argIndex, nil
	}
}

func (c *Converter) vertexPrefix() string {
	return fmt.Sprintf("[%s %s] ", c.mts.FinalStates.Target.String(), c.mts.FinalStates.Salt)
}

func (c *Converter) imageVertexPrefix(id string) string {
	h := fnv.New32a()
	h.Write([]byte(id))
	return fmt.Sprintf("[%s %d] ", id, h.Sum32())
}

func (c *Converter) vertexPrefixWithURL(url string) string {
	return fmt.Sprintf("[%s(%s) %s] ", c.mts.FinalStates.Target.String(), url, url)
}

func withDependency(state llb.State, target domain.Target, depState llb.State, depTarget domain.Target) llb.State {
	return llbutil.WithDependency(
		state, depState,
		llb.WithCustomNamef(
			"[internal] create artificial dependency: %s depends on %s",
			target.String(), depTarget.String()))
}

func makeCacheContext(target domain.Target) llb.State {
	sessionID := cacheKey(target)
	opts := []llb.LocalOption{
		llb.SharedKeyHint(target.ProjectCanonical()),
		llb.SessionID(sessionID),
		llb.Platform(llbutil.TargetPlatform),
		llb.WithCustomNamef("[internal] cache context %s", target.ProjectCanonical()),
	}
	return llb.Local("earthly-cache", opts...)
}

func cacheKey(target domain.Target) string {
	// Use the canonical target, but wihout the tag for cache matching.
	targetCopy := target
	targetCopy.Tag = ""
	digest := sha256.Sum256([]byte(targetCopy.StringCanonical()))
	return hex.EncodeToString(digest[:])
}

func joinWrap(a []string, before string, sep string, after string) string {
	if len(a) > 0 {
		return fmt.Sprintf("%s%s%s", before, strings.Join(a, sep), after)
	}
	return ""
}

func strIf(condition bool, str string) string {
	if condition {
		return str
	}
	return ""
}
