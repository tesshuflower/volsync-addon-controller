// +kubebuilder:validation:Required
package v1alpha1

import (
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const VolSyncAddOnConfigGroupVersionResourcePlural = "volsyncaddonconfigs"

var VolSyncAddOnConfigGVR = schema.GroupVersionResource{
	Group:    GroupVersion.Group,
	Version:  GroupVersion.Version,
	Resource: VolSyncAddOnConfigGroupVersionResourcePlural,
}

// VolSyncAddOnConfig defines the desired configuration for VolSync operator
// +kubebuilder:object:root=true
type VolSyncAddOnConfig struct {
	metav1.TypeMeta `json:",inline"`
	//+optional
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// spec is the desired configuration of the VolSync operator on the remote cluster
	Spec *VolSyncAddOnConfigSpec `json:"spec,omitempty"`
}

// VolSyncAddOnConfigList contains a list of VolSyncAddonConfig
// +kubebuilder:object:root=true
type VolSyncAddOnConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolSyncAddOnConfig `json:"items"`
}

type VolSyncAddOnConfigSpec struct {
	SubscriptionCatalogSource          string                                `json:"subscriptionSource,omitempty"`
	SubscriptionCatalogSourceNamespace string                                `json:"subscriptionSourceNamespace,omitempty"`
	SubscriptionChannel                string                                `json:"subscriptionChannel,omitempty"`
	SubscriptionStartingCSV            string                                `json:"subscriptionStartingCSV,omitempty"`
	SubscriptionInstallPlanApproval    operatorsv1alpha1.Approval            `json:"subscriptionInstallPlanApproval,omitempty"`
	SubscriptionConfig                 *operatorsv1alpha1.SubscriptionConfig `json:"subscriptionConfig,omitempty"`
}

func init() {
	SchemeBuilder.Register(&VolSyncAddOnConfig{}, &VolSyncAddOnConfigList{})
}
