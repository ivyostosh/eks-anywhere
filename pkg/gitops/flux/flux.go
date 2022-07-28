package flux

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/clustermarshaller"
	"github.com/aws/eks-anywhere/pkg/config"
	"github.com/aws/eks-anywhere/pkg/filewriter"
	"github.com/aws/eks-anywhere/pkg/git"
	gitFactory "github.com/aws/eks-anywhere/pkg/git/factory"
	"github.com/aws/eks-anywhere/pkg/logger"
	"github.com/aws/eks-anywhere/pkg/providers"
	"github.com/aws/eks-anywhere/pkg/retrier"
	"github.com/aws/eks-anywhere/pkg/templater"
	"github.com/aws/eks-anywhere/pkg/types"
	"github.com/aws/eks-anywhere/pkg/validations"
)

//go:embed manifests/eksa-system/kustomization.yaml
var eksaKustomizeContent string

//go:embed manifests/flux-system/kustomization.yaml
var fluxKustomizeContent string

//go:embed manifests/flux-system/gotk-sync.yaml
var fluxSyncContent string

//go:embed manifests/flux-system/gotk-patches.yaml
var fluxPatchContent string

const (
	maxRetries    = 5
	backOffPeriod = 5 * time.Second

	defaultRemote         = "origin"
	eksaSystemDirName     = "eksa-system"
	kustomizeFileName     = "kustomization.yaml"
	clusterConfigFileName = "eksa-cluster.yaml"
	fluxSyncFileName      = "gotk-sync.yaml"
	fluxPatchFileName     = "gotk-patches.yaml"

	initialClusterconfigCommitMessage = "Initial commit of cluster configuration; generated by EKS-A CLI"
	updateClusterconfigCommitMessage  = "Update commit of cluster configuration; generated by EKS-A CLI"
	deleteClusterconfigCommitMessage  = "Delete commit of cluster configuration; generated by EKS-A CLI"
)

type Flux struct {
	flux      Client
	gitTools  *gitFactory.GitTools
	cliConfig *config.CliConfig
	retrier   *retrier.Retrier
}

func NewFlux(flux Client, gitTools *gitFactory.GitTools, cliConfig *config.CliConfig) *Flux {
	return &Flux{
		flux:      flux,
		gitTools:  gitTools,
		cliConfig: cliConfig,
		retrier:   retrier.NewWithMaxRetries(maxRetries, backOffPeriod),
	}
}

// Client is an interface that abstracts the basic commands of flux executable.
type Client interface {
	// BootstrapGithub bootstraps toolkit components in a GitHub repository.
	BootstrapGithub(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig) error

	// BootstrapGit bootstraps toolkit componets in a generic Git repository
	BootstrapGit(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig, cliConfig *config.CliConfig) error

	// Uninstall removes the Flux components and the toolkit.fluxcd.io resources from the cluster.
	Uninstall(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig) error

	// SuspendKustomization pauses reconciliation of Kustomization
	SuspendKustomization(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig) error

	// ResumeKustomization resumes a paused Kustomization
	ResumeKustomization(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig) error

	// ForceReconcileGitRepo sync git repo with latest commit
	ForceReconcileGitRepo(ctx context.Context, cluster *types.Cluster, namespace string) error

	// DeleteFluxSystemSecret deletes flux-system secret
	DeleteFluxSystemSecret(ctx context.Context, cluster *types.Cluster, namespace string) error

	// Reconcile reconciles sources and resources
	Reconcile(ctx context.Context, cluster *types.Cluster, fluxConfig *v1alpha1.FluxConfig) error
}

func (f *Flux) SetRetier(retrier *retrier.Retrier) {
	f.retrier = retrier
}

func (f *Flux) ForceReconcileGitRepo(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps not configured, force reconcile flux git repo skipped")
		return nil
	}
	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	return f.flux.ForceReconcileGitRepo(ctx, cluster, fc.clusterSpec.FluxConfig.Spec.SystemNamespace)
}

// InstallGitOps validates and sets up the gitops/flux config, creates a repository if one doesn’t exist,
// commits the manifests for both eks-a cluster and flux components to the default branch at the specified path,
// and installs the Flux components. Then it configures the target cluster to synchronize with the specified path
// inside the repository.
func (f *Flux) InstallGitOps(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec, datacenterConfig providers.DatacenterConfig, machineConfigs []providers.MachineConfig) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps field not specified, bootstrap flux skipped")
		return nil
	}
	fc := &fluxForCluster{
		Flux:             f,
		clusterSpec:      clusterSpec,
		datacenterConfig: datacenterConfig,
		machineConfigs:   machineConfigs,
	}

	if clusterSpec.FluxConfig.Spec.Github != nil {
		err := f.installGitOpsGithub(ctx, cluster, fc, clusterSpec)
		if err != nil {
			return fmt.Errorf("installing GitHub gitops: %v", err)
		}
	}

	if clusterSpec.FluxConfig.Spec.Git != nil {
		err := f.installGitOpsGenericGit(ctx, cluster, fc, clusterSpec)
		if err != nil {
			return fmt.Errorf("installing generic git gitops: %v", err)
		}
	}

	logger.V(3).Info("pulling from remote after Flux Bootstrap to ensure configuration files in local git repository are in sync",
		"remote", defaultRemote, "branch", fc.branch())

	err := f.retrier.Retry(func() error {
		return f.gitTools.Client.Pull(ctx, fc.branch())
	})
	if err != nil {
		logger.Error(err, "error when pulling from remote repository after Flux Bootstrap; ensure local repository is up-to-date with remote (git pull)",
			"remote", defaultRemote, "branch", fc.branch(), "error", err)
	}
	return nil
}

func (f *Flux) installGitOpsGithub(ctx context.Context, cluster *types.Cluster, fc *fluxForCluster, clusterSpec *cluster.Spec) error {
	if err := fc.setupProviderRepository(ctx); err != nil {
		return err
	}

	if err := fc.commitFluxAndClusterConfigToGit(ctx); err != nil {
		return err
	}

	if !cluster.ExistingManagement {
		err := f.retrier.Retry(func() error {
			return fc.flux.BootstrapGithub(ctx, cluster, clusterSpec.FluxConfig)
		})
		if err != nil {
			uninstallErr := f.uninstallGitOpsToolkits(ctx, cluster, clusterSpec)
			if uninstallErr != nil {
				logger.Info("Could not uninstall flux components", "error", uninstallErr)
			}
			return err
		}
	}
	return nil
}

func (f *Flux) installGitOpsGenericGit(ctx context.Context, cluster *types.Cluster, fc *fluxForCluster, clusterSpec *cluster.Spec) error {
	err := fc.clone(ctx)
	if err != nil {
		return err
	}

	if err = fc.commitFluxAndClusterConfigToGit(ctx); err != nil {
		return err
	}

	if !cluster.ExistingManagement {
		err = f.retrier.Retry(func() error {
			return fc.flux.BootstrapGit(ctx, cluster, clusterSpec.FluxConfig, f.cliConfig)
		})
		if err != nil {
			uninstallErr := f.uninstallGitOpsToolkits(ctx, cluster, clusterSpec)
			if uninstallErr != nil {
				logger.Info("Could not uninstall flux components", "error", uninstallErr)
			}
			return err
		}
	}
	return nil
}

func (f *Flux) uninstallGitOpsToolkits(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	return f.retrier.Retry(func() error {
		return fc.flux.Uninstall(ctx, cluster, clusterSpec.FluxConfig)
	})
}

func (f *Flux) PauseGitOpsKustomization(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps field not specified, pause flux kustomization skipped")
		return nil
	}

	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	logger.V(3).Info("pause reconciliation of all Kustomization", "namespace", fc.namespace())

	return f.retrier.Retry(func() error {
		return fc.flux.SuspendKustomization(ctx, cluster, clusterSpec.FluxConfig)
	})
}

func (f *Flux) ResumeGitOpsKustomization(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps field not specified, resume flux kustomization skipped")
		return nil
	}

	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	logger.V(3).Info("resume reconciliation of all Kustomization", "namespace", fc.namespace())
	return f.retrier.Retry(func() error {
		return fc.flux.ResumeKustomization(ctx, cluster, clusterSpec.FluxConfig)
	})
}

func (f *Flux) UpdateGitEksaSpec(ctx context.Context, clusterSpec *cluster.Spec, datacenterConfig providers.DatacenterConfig, machineConfigs []providers.MachineConfig) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps field not specified, update git repo skipped")
		return nil
	}

	fc := &fluxForCluster{
		Flux:             f,
		clusterSpec:      clusterSpec,
		datacenterConfig: datacenterConfig,
		machineConfigs:   machineConfigs,
	}

	if err := fc.syncGitRepo(ctx); err != nil {
		return err
	}

	if err := fc.writeEksaSystemFiles(); err != nil {
		return err
	}

	path := fc.eksaSystemDir()
	err := f.gitTools.Client.Add(path)
	if err != nil {
		return &ConfigVersionControlFailedError{Err: fmt.Errorf("adding %s to git: %v", path, err)}
	}

	err = f.pushToRemoteRepo(ctx, path, updateClusterconfigCommitMessage)
	if err != nil {
		return err
	}
	logger.V(3).Info("Finished pushing updated cluster config file to git", "repository", fc.repository())
	return nil
}

func (f *Flux) Validations(ctx context.Context, clusterSpec *cluster.Spec) []validations.Validation {
	if f.shouldSkipFlux() {
		return nil
	}

	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	return []validations.Validation{
		func() *validations.ValidationResult {
			return &validations.ValidationResult{
				Name:        "Flux path",
				Remediation: "Please provide a different path or different cluster name",
				Err:         fc.validateRemoteConfigPathDoesNotExist(ctx),
			}
		},
	}
}

func (f *Flux) CleanupGitRepo(ctx context.Context, clusterSpec *cluster.Spec) error {
	if f.shouldSkipFlux() {
		logger.Info("GitOps field not specified, clean up git repo skipped")
		return nil
	}

	fc := &fluxForCluster{
		Flux:        f,
		clusterSpec: clusterSpec,
	}

	if err := fc.syncGitRepo(ctx); err != nil {
		return err
	}

	var p string
	if clusterSpec.Cluster.IsManaged() {
		p = fc.eksaSystemDir()
	} else {
		p = fc.path()
	}

	if !validations.FileExists(path.Join(f.gitTools.Writer.Dir(), p)) {
		logger.V(3).Info("cluster dir does not exist in git, skip clean up")
		return nil
	}

	err := f.gitTools.Client.Remove(p)
	if err != nil {
		return &ConfigVersionControlFailedError{Err: fmt.Errorf("removing %s in git: %v", p, err)}
	}

	err = f.pushToRemoteRepo(ctx, p, deleteClusterconfigCommitMessage)
	if err != nil {
		return err
	}
	logger.V(3).Info("Finished cleaning up cluster files in git",
		"repository", fc.repository())
	return nil
}

func (f *Flux) pushToRemoteRepo(ctx context.Context, path, msg string) error {
	err := f.gitTools.Client.Commit(msg)
	if err != nil {
		return &ConfigVersionControlFailedError{Err: fmt.Errorf("committing %s to git:  %v", path, err)}
	}

	err = f.retrier.Retry(func() error {
		return f.gitTools.Client.Push(ctx)
	})
	if err != nil {
		return &ConfigVersionControlFailedError{Err: fmt.Errorf("pushing %s to git: %v", path, err)}
	}
	return nil
}

type fluxForCluster struct {
	*Flux
	clusterSpec      *cluster.Spec
	datacenterConfig providers.DatacenterConfig
	machineConfigs   []providers.MachineConfig
}

// commitFluxAndClusterConfigToGit commits the cluster configuration file to the flux-managed git repository.
// If the remote repository does not exist it will initialize a local repository and push it to the configured remote.
// It will generate the kustomization file and marshal the cluster configuration file to the required locations in the repo.
// These will later be used by Flux and our controllers to reconcile the repository contents and the cluster configuration.
func (fc *fluxForCluster) commitFluxAndClusterConfigToGit(ctx context.Context) error {
	logger.Info("Adding cluster configuration files to Git")
	config := fc.clusterSpec.FluxConfig

	err := fc.validateLocalConfigPathDoesNotExist()
	if err != nil {
		return &ConfigVersionControlFailedError{Err: err}
	}

	err = fc.writeEksaSystemFiles()
	if err != nil {
		return &ConfigVersionControlFailedError{Err: err}
	}

	if fc.clusterSpec.Cluster.IsSelfManaged() {
		err = fc.writeFluxSystemFiles()
		if err != nil {
			return &ConfigVersionControlFailedError{Err: err}
		}

	} else {
		logger.V(3).Info("Skipping flux custom manifest files")
	}
	p := path.Dir(config.Spec.ClusterConfigPath)
	err = fc.gitTools.Client.Add(p)
	if err != nil {
		return &ConfigVersionControlFailedError{Err: fmt.Errorf("adding %s to git: %v", p, err)}
	}

	err = fc.Flux.pushToRemoteRepo(ctx, p, initialClusterconfigCommitMessage)
	if err != nil {
		return err
	}
	logger.V(3).Info("Finished pushing cluster config and flux custom manifest files to git")
	return nil
}

func (fc *fluxForCluster) syncGitRepo(ctx context.Context) error {
	f := fc.Flux
	if !validations.FileExists(path.Join(f.gitTools.Writer.Dir(), ".git")) {
		err := fc.clone(ctx)
		if err != nil {
			return fmt.Errorf("failed cloning git repo: %v", err)
		}
	} else {
		// Make sure the local git repo is on the branch specified in config and up-to-date with the remote
		if err := fc.gitTools.Client.Branch(fc.branch()); err != nil {
			return fmt.Errorf("failed to switch to git branch %s: %v", fc.branch(), err)
		}
	}
	return nil
}

func (fc *fluxForCluster) initEksaWriter() (filewriter.FileWriter, error) {
	eksaSystemDir := fc.eksaSystemDir()
	w, err := fc.gitTools.Writer.WithDir(eksaSystemDir)
	if err != nil {
		err = fmt.Errorf("creating %s directory: %v", eksaSystemDir, err)
	}
	w.CleanUpTemp()
	return w, err
}

func (fc *fluxForCluster) writeEksaSystemFiles() error {
	if fc.datacenterConfig == nil && fc.machineConfigs == nil {
		return nil
	}

	logger.V(3).Info("Generating eks-a cluster manifest files...")
	w, err := fc.initEksaWriter()
	if err != nil {
		return err
	}

	logger.V(4).Info("Generating eks-a cluster config file...")
	if err := fc.generateClusterConfigFile(w); err != nil {
		return err
	}

	logger.V(4).Info("Generating eks-a kustomization file...")
	return fc.generateEksaKustomizeFile(w)
}

func (fc *fluxForCluster) generateClusterConfigFile(w filewriter.FileWriter) error {
	resourcesSpec, err := clustermarshaller.MarshalClusterSpec(fc.clusterSpec, fc.datacenterConfig, fc.machineConfigs)
	if err != nil {
		return err
	}
	if filePath, err := w.Write(clusterConfigFileName, resourcesSpec, filewriter.PersistentFile); err != nil {
		return fmt.Errorf("writing eks-a cluster config file into %s: %v", filePath, err)
	}

	return nil
}

func (fc *fluxForCluster) generateEksaKustomizeFile(w filewriter.FileWriter) error {
	values := map[string]string{
		"ConfigFileName": clusterConfigFileName,
	}
	t := templater.New(w)
	if filePath, err := t.WriteToFile(eksaKustomizeContent, values, kustomizeFileName, filewriter.PersistentFile); err != nil {
		return fmt.Errorf("writing eks-a kustomization manifest file into %s: %v", filePath, err)
	}
	return nil
}

func (fc *fluxForCluster) initFluxWriter() (filewriter.FileWriter, error) {
	w, err := fc.gitTools.Writer.WithDir(fc.fluxSystemDir())
	if err != nil {
		err = fmt.Errorf("creating %s directory: %v", fc.fluxSystemDir(), err)
	}
	w.CleanUpTemp()
	return w, err
}

func (fc *fluxForCluster) writeFluxSystemFiles() (err error) {
	logger.V(3).Info("Generating flux custom manifest files...")
	w, err := fc.initFluxWriter()
	if err != nil {
		return err
	}

	t := templater.New(w)

	logger.V(4).Info("Generating flux-system kustomization file...")
	if err = fc.generateFluxKustomizeFile(t); err != nil {
		return err
	}

	logger.V(4).Info("Generating flux-system sync file...")
	if err = fc.generateFluxSyncFile(t); err != nil {
		return err
	}

	logger.V(3).Info("Generating flux-system patch file...")
	if err = fc.generateFluxPatchFile(t); err != nil {
		return err
	}

	return nil
}

func (fc *fluxForCluster) generateFluxKustomizeFile(t *templater.Templater) error {
	values := map[string]string{
		"Namespace": fc.namespace(),
	}
	if filePath, err := t.WriteToFile(fluxKustomizeContent, values, kustomizeFileName, filewriter.PersistentFile); err != nil {
		return fmt.Errorf("creating flux-system kustomization manifest file into %s: %v", filePath, err)
	}
	return nil
}

func (f *Flux) generateFluxSyncFile(t *templater.Templater) error {
	if filePath, err := t.WriteToFile(fluxSyncContent, nil, fluxSyncFileName, filewriter.PersistentFile); err != nil {
		return fmt.Errorf("creating flux-system sync manifest file into %s: %v", filePath, err)
	}
	return nil
}

func (fc *fluxForCluster) generateFluxPatchFile(t *templater.Templater) error {
	bundle := fc.clusterSpec.VersionsBundle
	values := map[string]string{
		"Namespace":                   fc.namespace(),
		"SourceControllerImage":       bundle.Flux.SourceController.VersionedImage(),
		"KustomizeControllerImage":    bundle.Flux.KustomizeController.VersionedImage(),
		"HelmControllerImage":         bundle.Flux.HelmController.VersionedImage(),
		"NotificationControllerImage": bundle.Flux.NotificationController.VersionedImage(),
	}
	if filePath, err := t.WriteToFile(fluxPatchContent, values, fluxPatchFileName, filewriter.PersistentFile); err != nil {
		return fmt.Errorf("creating flux-system patch manifest file into %s: %v", filePath, err)
	}
	return nil
}

// setupProviderRepository will set up the repository which will house the GitOps configuration for the cluster.
// if the repository exists and is not empty, it will be cloned.
// if the repository exists but is empty, it will be initialized locally, as a bare repository cannot be cloned.
// if the repository does not exist, it will be created and then initialized locally.
func (fc *fluxForCluster) setupProviderRepository(ctx context.Context) error {
	var r *git.Repository
	var err error
	err = fc.Flux.retrier.Retry(func() error {
		r, err = fc.gitTools.Provider.GetRepo(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to describe repo: %w", err)
	}
	if r != nil {
		err = fc.clone(ctx)
	}
	if err != nil {
		var repoEmptyErr *git.RepositoryIsEmptyError
		if errors.As(err, &repoEmptyErr) {
			logger.V(3).Info("remote repository is empty and can't be cloned; will initialize locally")
			if err = fc.initializeLocalRepository(); err != nil {
				return &ConfigVersionControlFailedError{err}
			}
			return nil
		}
		return &ConfigVersionControlFailedError{Err: err}
	}

	if r == nil {
		if err = fc.createRemoteRepository(ctx); err != nil {
			return &ConfigVersionControlFailedError{err}
		}

		if err = fc.initializeLocalRepository(); err != nil {
			return &ConfigVersionControlFailedError{err}
		}
	}

	return nil
}

func (fc *fluxForCluster) clone(ctx context.Context) error {
	logger.V(3).Info("Cloning remote repository")
	err := fc.Flux.retrier.Retry(func() error {
		return fc.gitTools.Client.Clone(ctx)
	})
	if err != nil {
		return err
	}

	logger.V(3).Info("Creating a new branch")
	err = fc.gitTools.Client.Branch(fc.branch())
	if err != nil {
		return err
	}
	return nil
}

// createRemoteRepository will create a repository in the remote git provider with the user-provided configuration
func (fc *fluxForCluster) createRemoteRepository(ctx context.Context) error {
	n := fc.repository()
	o := fc.owner()
	p := fc.personal()
	d := "EKS-A cluster configuration repository"
	logger.V(3).Info("Remote Github repo does not exist; will create and initialize", "repo", n, "owner", o)

	opts := git.CreateRepoOpts{Name: n, Owner: o, Description: d, Personal: p, Privacy: true}
	logger.V(3).Info("Creating remote Github repo", "options", opts)
	err := fc.Flux.retrier.Retry(func() error {
		_, err := fc.gitTools.Provider.CreateRepo(ctx, opts)
		return err
	})
	if err != nil {
		return fmt.Errorf("could not create repo: %w", err)
	}
	return nil
}

// initializeLocalRepository will git init the local repository directory, initialize a git repository.
// it will then change branches to the branch specified in the GitOps configuration
func (fc *fluxForCluster) initializeLocalRepository() error {
	err := fc.gitTools.Client.Init()
	if err != nil {
		return fmt.Errorf("could not initialize repo: %w", err)
	}

	// git requires at least one commit in the repo to branch from
	if err = fc.gitTools.Client.Commit("initializing repository"); err != nil {
		return fmt.Errorf("initializing repository: %v", err)
	}

	if err = fc.gitTools.Client.Branch(fc.branch()); err != nil {
		return fmt.Errorf("creating branch: %v", err)
	}
	return nil
}

// validateLocalConfigPathDoesNotExist returns an exception if the cluster configuration file exists.
// This is done so that we avoid clobbering existing cluster configurations in the user-provided git repository.
func (fc *fluxForCluster) validateLocalConfigPathDoesNotExist() error {
	if fc.clusterSpec.Cluster.IsSelfManaged() {
		p := path.Join(fc.gitTools.Writer.Dir(), fc.path())
		if validations.FileExists(p) {
			return fmt.Errorf("a cluster configuration file already exists at path %s", p)
		}
	}
	return nil
}

func (fc *fluxForCluster) validateRemoteConfigPathDoesNotExist(ctx context.Context) error {
	if fc.clusterSpec.Cluster.IsSelfManaged() {
		if fc.gitTools.Provider != nil {
			if exists, err := fc.gitTools.Provider.PathExists(ctx, fc.owner(), fc.repository(), fc.branch(), fc.path()); err != nil {
				return fmt.Errorf("failed validating remote flux config path: %v", err)
			} else if exists {
				return fmt.Errorf("flux path %s already exists in remote repository", fc.path())
			}
		}
	}
	return nil
}

func (fc *fluxForCluster) namespace() string {
	return fc.clusterSpec.FluxConfig.Spec.SystemNamespace
}

func (fc *fluxForCluster) repository() string {
	if fc.clusterSpec.FluxConfig.Spec.Github != nil {
		return fc.clusterSpec.FluxConfig.Spec.Github.Repository
	}
	if fc.clusterSpec.FluxConfig.Spec.Git != nil {
		r := fc.clusterSpec.FluxConfig.Spec.Git.RepositoryUrl
		return path.Base(strings.TrimSuffix(r, filepath.Ext(r)))
	}
	return ""
}

func (fc *fluxForCluster) owner() string {
	if fc.clusterSpec.FluxConfig.Spec.Github != nil {
		return fc.clusterSpec.FluxConfig.Spec.Github.Owner
	}
	return ""
}

func (fc *fluxForCluster) branch() string {
	return fc.clusterSpec.FluxConfig.Spec.Branch
}

func (fc *fluxForCluster) personal() bool {
	if fc.clusterSpec.FluxConfig.Spec.Github != nil {
		return fc.clusterSpec.FluxConfig.Spec.Github.Personal
	}
	return false
}

func (fc *fluxForCluster) path() string {
	return fc.clusterSpec.FluxConfig.Spec.ClusterConfigPath
}

type ConfigVersionControlFailedError struct {
	Err error
}

func (e *ConfigVersionControlFailedError) Error() string {
	return fmt.Sprintf("Encountered an error when attempting to version control cluster config: %v", e.Err)
}

func (fc *fluxForCluster) eksaSystemDir() string {
	return path.Join(fc.path(), fc.clusterSpec.Cluster.GetName(), eksaSystemDirName)
}

func (fc *fluxForCluster) fluxSystemDir() string {
	return path.Join(fc.path(), fc.namespace())
}

func (f *Flux) shouldSkipFlux() bool {
	return f.gitTools == nil
}