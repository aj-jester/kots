package k8sdoc

type Doc struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type Spec struct {
	Template    Template    `yaml:"template,omitempty"`
	JobTemplate JobTemplate `yaml:"jobTemplate,omitempty"`
}

type JobTemplate struct {
	Spec JobSpec `yaml:"spec"`
}

type JobSpec struct {
	Template Template `yaml:"template"`
}

type Template struct {
	Spec PodSpec `yaml:"spec"`
}

type PodSpec struct {
	Containers       []Container       `yaml:"containers,omitempty"`     // don't write empty array into patches
	InitContainers   []Container       `yaml:"initContainers,omitempty"` // don't write empty array into patches
	ImagePullSecrets []ImagePullSecret `yaml:"imagePullSecrets"`
}

type ImagePullSecret map[string]string

type Container struct {
	Image string `yaml:"image"`
}
