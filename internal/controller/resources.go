package controller

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/krishau99/docs/api/v1alpha1"
)

const (
	// Volume and mount names
	volumeWorkspace = "workspace"
	volumeOutput    = "output"
	volumeCASecret  = "ca-cert"

	// Mount paths
	mountWorkspace = "/workspace"
	mountOutput    = "/output"
	mountCA        = "/etc/ssl/certs/custom-ca.crt"

	// Default build toolchain images (can be overridden by prepending registry.url).
	// There is deliberately no default serving image: the serving container has
	// to listen on spec.serving.port, and only whoever configures the DocsPage
	// knows which image does.
	defaultGitImage   = "alpine/git:latest"
	defaultBuildImage = "zensical/zensical:latest"

	// Fallback document root, matching the layout of httpd-based images.
	defaultDocumentRoot = "/usr/local/apache2/htdocs"

	// Annotation used to trigger rolling restarts
	annotationRestartedAt = "docspage/restartedAt"
	// Annotation for tracking current SHA on Deployment
	annotationCurrentSHA = "docspage/currentSHA"
)

// prefixImage prepends the registry URL to an image name if the image doesn't
// already contain a registry hostname (i.e., a '/' preceded by a hostname with a dot or port).
func prefixImage(registryURL, image string) string {
	if registryURL == "" {
		return image
	}
	// Only treat the first component as a registry hostname when the image
	// contains a '/' (i.e., has a path component). Bare image names like
	// "httpd:2.4-alpine" have no '/', so their ':' is a tag separator, not
	// a registry port, and the registry URL must still be prepended.
	parts := strings.SplitN(image, "/", 2)
	if len(parts) > 1 {
		firstComponent := parts[0]
		if strings.Contains(firstComponent, ".") || strings.Contains(firstComponent, ":") {
			return image
		}
	}
	return registryURL + "/" + image
}

// ptrTo returns a pointer to v. The Kubernetes API types use pointers for
// optional booleans so that "unset" stays distinguishable from false, and the
// security contexts below need several of them.
func ptrTo[T any](v T) *T { return &v }

// restrictedPodSecurityContext returns the pod-level half of a security context
// satisfying the restricted Pod Security Standard.
//
// RunAsUser is set explicitly rather than left to the images. Restricted
// requires RunAsNonRoot, and the kubelet refuses to start a container whose
// image would run as root when that is set; alpine/git declares no USER, so
// without an explicit UID a build-mode pod would never start at all.
//
// FSGroup is deliberately not set. The emptyDir volumes the containers share
// are created mode 0777, so every container can write to them whichever UID it
// runs as, and setting FSGroup would additionally rewrite the ownership of the
// mounted CA certificate to no purpose.
func restrictedPodSecurityContext(dp *v1alpha1.DocsPage) *corev1.PodSecurityContext {
	uid := runAsUser(dp)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptrTo(true),
		RunAsUser:    &uid,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// restrictedContainerSecurityContext returns the container-level half. It is
// the same for every container including the init containers, because the
// standard is evaluated per container and one non-compliant container makes the
// whole pod inadmissible.
//
// ReadOnlyRootFilesystem is not set. It is not part of the restricted standard,
// and all three containers write outside their mounts — git to its temporary
// files, zensical to its cache, httpd to /var/run and its logs.
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptrTo(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// labelsForDocsPage returns standard labels for resources owned by a DocsPage.
func labelsForDocsPage(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "docspage",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "docspage-operator",
	}
}

// buildDeployment constructs the Deployment object for the given DocsPage.
func buildDeployment(dp *v1alpha1.DocsPage, registryURL, currentSHA string) *appsv1.Deployment {
	labels := labelsForDocsPage(dp.Name)

	replicas := int32(1)
	if dp.Spec.Serving.Replicas != nil {
		replicas = *dp.Spec.Serving.Replicas
	}

	port := servingPort(dp)

	var podSpec corev1.PodSpec
	if dp.Spec.Mode == v1alpha1.DocsPageModeBuild {
		podSpec = buildPodSpecForBuildMode(dp, registryURL, port)
	} else {
		podSpec = buildPodSpecForPrebuiltMode(dp, registryURL, port)
	}

	annotations := map[string]string{}
	if currentSHA != "" {
		annotations[annotationCurrentSHA] = currentSHA
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dp.Name,
			Namespace: dp.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: podSpec,
			},
		},
	}
}

// buildPodSpecForBuildMode builds the pod spec for mode=build.
// Volumes:
//   - workspace (emptyDir): shared between init containers and main container
//   - output (emptyDir): built documentation, mounted into the serving container
//   - ca-cert (secret volume): optional custom CA certificate
func buildPodSpecForBuildMode(dp *v1alpha1.DocsPage, registryURL string, port int32) corev1.PodSpec {
	gitImageRef := defaultGitImage
	buildImageRef := defaultBuildImage

	if dp.Spec.Images != nil {
		if dp.Spec.Images.Git != "" {
			gitImageRef = dp.Spec.Images.Git
		}
		if dp.Spec.Images.Zensical != "" {
			buildImageRef = dp.Spec.Images.Zensical
		}
	}

	gitImage := prefixImage(registryURL, gitImageRef)
	buildImage := prefixImage(registryURL, buildImageRef)
	// Validated by the reconciler before we get here.
	serveImage := prefixImage(registryURL, dp.Spec.Serving.Image)

	volumes := []corev1.Volume{
		{
			Name: volumeWorkspace,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: volumeOutput,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// CA certificate volume
	caSecretName := ""
	if dp.Spec.TLS != nil {
		caSecretName = dp.Spec.TLS.CASecret
	}
	if caSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: volumeCASecret,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: caSecretName,
					Items: []corev1.KeyToPath{
						{Key: "ca.crt", Path: "custom-ca.crt"},
					},
				},
			},
		})
	}

	caVolumeMount := corev1.VolumeMount{
		Name:      volumeCASecret,
		MountPath: mountCA,
		SubPath:   "custom-ca.crt",
	}
	caEnvVar := corev1.EnvVar{
		Name:  "SSL_CERT_FILE",
		Value: mountCA,
	}

	// Init container 1: git clone
	gitInitContainer := corev1.Container{
		Name:  "git-clone",
		Image: gitImage,
		Command: []string{
			"sh", "-c",
			buildGitCloneCommand(dp),
		},
		SecurityContext: restrictedContainerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeWorkspace, MountPath: mountWorkspace},
		},
		Env: []corev1.EnvVar{},
	}
	if caSecretName != "" {
		gitInitContainer.VolumeMounts = append(gitInitContainer.VolumeMounts, caVolumeMount)
		gitInitContainer.Env = append(gitInitContainer.Env, caEnvVar)
		gitInitContainer.Env = append(gitInitContainer.Env, corev1.EnvVar{
			Name:  "GIT_SSL_CAINFO",
			Value: mountCA,
		})
	}

	// Add git credentials from secret if specified
	if dp.Spec.Repo != nil && dp.Spec.Repo.CredentialsSecret != "" {
		gitInitContainer.Env = append(gitInitContainer.Env,
			corev1.EnvVar{
				Name: "GIT_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: dp.Spec.Repo.CredentialsSecret,
						},
						Key: "username",
					},
				},
			},
			corev1.EnvVar{
				Name: "GIT_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: dp.Spec.Repo.CredentialsSecret,
						},
						Key: "password",
					},
				},
			},
		)
	}

	// Init container 2: variable substitution + Zensical build
	buildEnvVars := buildEnvSubstVars(dp)
	if caSecretName != "" {
		buildEnvVars = append(buildEnvVars, caEnvVar)
	}

	buildInitContainer := corev1.Container{
		Name:            "zensical-build",
		Image:           buildImage,
		Command:         []string{"sh", "-c", buildZensicalBuildCommand()},
		SecurityContext: restrictedContainerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeWorkspace, MountPath: mountWorkspace},
			{Name: volumeOutput, MountPath: mountOutput},
		},
		Env: buildEnvVars,
	}
	if caSecretName != "" {
		buildInitContainer.VolumeMounts = append(buildInitContainer.VolumeMounts, caVolumeMount)
	}

	// Main container: the caller-supplied image serving the built output.
	// Its listen port is not configured here — see ServingSpec.
	serveContainer := corev1.Container{
		Name:            "serve",
		Image:           serveImage,
		SecurityContext: restrictedContainerSecurityContext(),
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeOutput, MountPath: documentRoot(dp)},
		},
		Env: []corev1.EnvVar{},
	}
	if caSecretName != "" {
		serveContainer.VolumeMounts = append(serveContainer.VolumeMounts, caVolumeMount)
		serveContainer.Env = append(serveContainer.Env, caEnvVar)
	}

	return corev1.PodSpec{
		SecurityContext: restrictedPodSecurityContext(dp),
		InitContainers: []corev1.Container{
			gitInitContainer,
			buildInitContainer,
		},
		Containers: []corev1.Container{
			serveContainer,
		},
		Volumes: volumes,
	}
}

// buildPodSpecForPrebuiltMode builds the pod spec for mode=prebuilt.
func buildPodSpecForPrebuiltMode(dp *v1alpha1.DocsPage, registryURL string, port int32) corev1.PodSpec {
	// Validated by the reconciler before we get here.
	image := prefixImage(registryURL, dp.Spec.Image)

	volumes := []corev1.Volume{}
	caSecretName := ""
	if dp.Spec.TLS != nil {
		caSecretName = dp.Spec.TLS.CASecret
	}

	containerEnv := []corev1.EnvVar{}
	containerVolumeMounts := []corev1.VolumeMount{}

	if caSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: volumeCASecret,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: caSecretName,
					Items: []corev1.KeyToPath{
						{Key: "ca.crt", Path: "custom-ca.crt"},
					},
				},
			},
		})
		containerVolumeMounts = append(containerVolumeMounts, corev1.VolumeMount{
			Name:      volumeCASecret,
			MountPath: mountCA,
			SubPath:   "custom-ca.crt",
		})
		containerEnv = append(containerEnv, corev1.EnvVar{
			Name:  "SSL_CERT_FILE",
			Value: mountCA,
		})
	}

	mainContainer := corev1.Container{
		Name:            "docs",
		Image:           image,
		SecurityContext: restrictedContainerSecurityContext(),
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		Env:          containerEnv,
		VolumeMounts: containerVolumeMounts,
	}

	return corev1.PodSpec{
		SecurityContext: restrictedPodSecurityContext(dp),
		Containers:      []corev1.Container{mainContainer},
		Volumes:         volumes,
	}
}

// buildService constructs the ClusterIP Service for the given DocsPage.
func buildService(dp *v1alpha1.DocsPage) *corev1.Service {
	labels := labelsForDocsPage(dp.Name)

	port := servingPort(dp)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dp.Name,
			Namespace: dp.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// buildGitCloneCommand generates the shell command used in the git clone init container.
// It supports authentication via GIT_USERNAME/GIT_PASSWORD environment variables.
func buildGitCloneCommand(dp *v1alpha1.DocsPage) string {
	if dp.Spec.Repo == nil {
		return "echo 'No repository configured'; exit 1"
	}

	branch := dp.Spec.Repo.Branch
	if branch == "" {
		branch = "main"
	}

	repoURL := dp.Spec.Repo.URL

	if dp.Spec.Repo.CredentialsSecret != "" {
		// Inject credentials into the URL
		// Replaces https://host/path with https://user:pass@host/path
		return fmt.Sprintf(
			`set -e
REPO_URL="%s"
if echo "$REPO_URL" | grep -q "^https://"; then
  AUTHENTICATED_URL=$(echo "$REPO_URL" | sed "s|https://|https://${GIT_USERNAME}:${GIT_PASSWORD}@|")
elif echo "$REPO_URL" | grep -q "^http://"; then
  AUTHENTICATED_URL=$(echo "$REPO_URL" | sed "s|http://|http://${GIT_USERNAME}:${GIT_PASSWORD}@|")
else
  AUTHENTICATED_URL="$REPO_URL"
fi
git clone --depth 1 --branch %s "$AUTHENTICATED_URL" %s`,
			repoURL, branch, mountWorkspace,
		)
	}

	return fmt.Sprintf(
		"set -e\ngit clone --depth 1 --branch %s %s %s",
		branch, repoURL, mountWorkspace,
	)
}

// buildZensicalBuildCommand generates the shell command used in the Zensical build init container.
// It runs envsubst on all .md and .toml files in the workspace, then builds with Zensical.
// Zensical outputs to ./site (i.e. /workspace/site) by default; the built files are then
// moved to the output volume (/output), which is mounted into the serving container.
func buildZensicalBuildCommand() string {
	return `set -e
cd ` + mountWorkspace + `

# Substitute environment variables in all markdown and TOML files
find . -type f \( -name "*.md" -o -name "*.toml" -o -name "*.html" \) | while read -r file; do
  envsubst < "$file" > "$file.tmp" && mv "$file.tmp" "$file"
done

# Build with Zensical (default output directory is ./site)
zensical build

# Move the built files (including hidden files) from the default ./site output directory to the output volume
if [ -d ` + mountWorkspace + `/site ]; then
  find ` + mountWorkspace + `/site/. -maxdepth 1 ! -name '.' -exec mv -t ` + mountOutput + `/ {} +
fi`
}

// buildEnvSubstVars converts the DocsPage variables and extraSubstitutions into
// Kubernetes environment variables for use in the build init container.
func buildEnvSubstVars(dp *v1alpha1.DocsPage) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	for k, v := range dp.Spec.Variables {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	for k, v := range dp.Spec.ExtraSubstitutions {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	return envVars
}
