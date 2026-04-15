package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DocsPageMode defines the operating mode of a DocsPage.
// +kubebuilder:validation:Enum=build;prebuilt
type DocsPageMode string

const (
	// DocsPageModeBuild clones a git repo, substitutes variables, builds with Zensical, and serves with Apache.
	DocsPageModeBuild DocsPageMode = "build"
	// DocsPageModePrebuilt deploys a pre-existing documentation image directly.
	DocsPageModePrebuilt DocsPageMode = "prebuilt"
)

// RepoSpec defines the git repository configuration for build mode.
type RepoSpec struct {
	// URL is the URL of the git repository to clone.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Branch is the branch to clone and monitor for changes.
	// +kubebuilder:default=main
	Branch string `json:"branch,omitempty"`

	// CredentialsSecret is the name of a Kubernetes Secret containing git credentials.
	// The secret should contain keys: username and password (or token).
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// RegistrySpec defines the container registry configuration.
type RegistrySpec struct {
	// URL is the registry URL prefix to prepend to all image references.
	// Example: "registry.internal"
	// +optional
	URL string `json:"url,omitempty"`
}

// TLSSpec defines TLS/CA certificate configuration.
type TLSSpec struct {
	// CASecret is the name of a Kubernetes Secret containing a custom CA certificate.
	// The secret should contain a key "ca.crt" with the PEM-encoded CA certificate.
	// This certificate is used for TLS verification when connecting to the registry,
	// Gitea, and other services.
	// +optional
	CASecret string `json:"caSecret,omitempty"`
}

// ServingSpec defines the serving configuration for the documentation.
type ServingSpec struct {
	// Replicas is the number of replicas for the serving Deployment.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Port is the port on which the documentation is served.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
}

// DocsPageSpec defines the desired state of DocsPage.
type DocsPageSpec struct {
	// Mode defines how this documentation page is deployed.
	// "build" clones a git repo and builds with Zensical.
	// "prebuilt" deploys a pre-existing documentation image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=build;prebuilt
	Mode DocsPageMode `json:"mode"`

	// Image is the pre-built documentation image to deploy.
	// Only used when mode is "prebuilt".
	// +optional
	Image string `json:"image,omitempty"`

	// Repo defines the git repository configuration.
	// Only used when mode is "build".
	// +optional
	Repo *RepoSpec `json:"repo,omitempty"`

	// PollInterval is how often the controller checks Gitea for new commits.
	// Only used when mode is "build".
	// Format: duration string, e.g. "5m", "1h"
	// +kubebuilder:default="5m"
	// +optional
	PollInterval string `json:"pollInterval,omitempty"`

	// Variables are key-value pairs substituted into documentation files and zensical.toml.
	// Variables are available as ${KEY} in files processed by envsubst.
	// Only used when mode is "build".
	// +optional
	Variables map[string]string `json:"variables,omitempty"`

	// ExtraSubstitutions are additional key-value pairs for variable substitution,
	// supplementing the Variables field with custom values.
	// Only used when mode is "build".
	// +optional
	ExtraSubstitutions map[string]string `json:"extraSubstitutions,omitempty"`

	// Registry defines the container registry configuration.
	// +optional
	Registry *RegistrySpec `json:"registry,omitempty"`

	// TLS defines the TLS/CA certificate configuration.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// Serving defines the serving configuration.
	// +optional
	Serving ServingSpec `json:"serving,omitempty"`
}

// DocsPageStatus defines the observed state of DocsPage.
type DocsPageStatus struct {
	// CurrentSHA is the git commit SHA of the currently deployed documentation.
	// Only set when mode is "build".
	// +optional
	CurrentSHA string `json:"currentSHA,omitempty"`

	// LastSyncTime is the time of the last successful synchronization.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Ready indicates whether the documentation is ready to serve traffic.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Conditions represent the latest available observations of the DocsPage state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=docs,shortName=dp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="SHA",type=string,JSONPath=`.status.currentSHA`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DocsPage is the Schema for the docspages API.
// It manages documentation page deployments in Kubernetes, supporting both
// build-from-source (Zensical) and pre-built image modes.
type DocsPage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DocsPageSpec   `json:"spec,omitempty"`
	Status DocsPageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DocsPageList contains a list of DocsPage.
type DocsPageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DocsPage `json:"items"`
}
