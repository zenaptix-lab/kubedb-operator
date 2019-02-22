package admission

import (
	"fmt"
	"strings"
	"sync"

	"github.com/appscode/go/log"
	hookapi "github.com/appscode/kubernetes-webhook-util/admission/v1beta1"
	meta_util "github.com/appscode/kutil/meta"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	cs "github.com/kubedb/apimachinery/client/clientset/versioned"
	amv "github.com/kubedb/apimachinery/pkg/validator"
	"github.com/pkg/errors"
	admission "k8s.io/api/admission/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type MemcachedValidator struct {
	client      kubernetes.Interface
	extClient   cs.Interface
	lock        sync.RWMutex
	initialized bool
}

var _ hookapi.AdmissionHook = &MemcachedValidator{}

// No forbidden envs yet
var forbiddenEnvVars []string

func (a *MemcachedValidator) Resource() (plural schema.GroupVersionResource, singular string) {
	return schema.GroupVersionResource{
			Group:    "validators.kubedb.com",
			Version:  "v1alpha1",
			Resource: "memcacheds",
		},
		"memcached"
}

func (a *MemcachedValidator) Initialize(config *rest.Config, stopCh <-chan struct{}) error {
	a.lock.Lock()
	defer a.lock.Unlock()

	a.initialized = true

	var err error
	if a.client, err = kubernetes.NewForConfig(config); err != nil {
		return err
	}
	if a.extClient, err = cs.NewForConfig(config); err != nil {
		return err
	}
	return err
}

func (a *MemcachedValidator) Admit(req *admission.AdmissionRequest) *admission.AdmissionResponse {
	status := &admission.AdmissionResponse{}

	if (req.Operation != admission.Create && req.Operation != admission.Update && req.Operation != admission.Delete) ||
		len(req.SubResource) != 0 ||
		req.Kind.Group != api.SchemeGroupVersion.Group ||
		req.Kind.Kind != api.ResourceKindMemcached {
		status.Allowed = true
		return status
	}

	a.lock.RLock()
	defer a.lock.RUnlock()
	if !a.initialized {
		return hookapi.StatusUninitialized()
	}

	switch req.Operation {
	case admission.Delete:
		if req.Name != "" {
			// req.Object.Raw = nil, so read from kubernetes
			obj, err := a.extClient.KubedbV1alpha1().Memcacheds(req.Namespace).Get(req.Name, metav1.GetOptions{})
			if err != nil && !kerr.IsNotFound(err) {
				return hookapi.StatusInternalServerError(err)
			} else if err == nil && obj.Spec.TerminationPolicy == api.TerminationPolicyDoNotTerminate {
				return hookapi.StatusBadRequest(fmt.Errorf(`memcached "%v/%v" can't be paused. To delete, change spec.terminationPolicy`, req.Namespace, req.Name))
			}
		}
	default:
		obj, err := meta_util.UnmarshalFromJSON(req.Object.Raw, api.SchemeGroupVersion)
		if err != nil {
			return hookapi.StatusBadRequest(err)
		}
		if req.Operation == admission.Update {
			// validate changes made by user
			oldObject, err := meta_util.UnmarshalFromJSON(req.OldObject.Raw, api.SchemeGroupVersion)
			if err != nil {
				return hookapi.StatusBadRequest(err)
			}
			memcached := obj.(*api.Memcached).DeepCopy()
			oldMemcached := oldObject.(*api.Memcached).DeepCopy()
			oldMemcached.SetDefaults()
			if err := validateUpdate(memcached, oldMemcached, req.Kind.Kind); err != nil {
				return hookapi.StatusBadRequest(fmt.Errorf("%v", err))
			}
		}
		// validate database specs
		if err = ValidateMemcached(a.client, a.extClient, obj.(*api.Memcached), false); err != nil {
			return hookapi.StatusForbidden(err)
		}
	}
	status.Allowed = true
	return status
}

// ValidateMemcached checks if the object satisfies all the requirements.
// It is not method of Interface, because it is referenced from controller package too.
func ValidateMemcached(client kubernetes.Interface, extClient cs.Interface, memcached *api.Memcached, strictValidation bool) error {
	if memcached.Spec.Version == "" {
		return fmt.Errorf(`object 'Version' is missing in '%v'`, memcached.Spec)
	}

	// Check Memcached version validation
	if _, err := extClient.CatalogV1alpha1().MemcachedVersions().Get(string(memcached.Spec.Version), metav1.GetOptions{}); err != nil {
		return err
	}

	if strictValidation {
		// Check if memcachedVersion is deprecated.
		// If deprecated, return error
		memcachedVersion, err := extClient.CatalogV1alpha1().MemcachedVersions().Get(string(memcached.Spec.Version), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if memcachedVersion.Spec.Deprecated {
			return fmt.Errorf("memcached %s/%s is using deprecated version %v. Skipped processing",
				memcached.Namespace, memcached.Name, memcachedVersion.Name)
		}
	}

	if err := amv.ValidateEnvVar(memcached.Spec.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMemcached); err != nil {
		return err
	}

	if memcached.Spec.Replicas == nil || *memcached.Spec.Replicas < 1 {
		return fmt.Errorf(`spec.replicas "%v" invalid. Value must be atleast 1"`, memcached.Spec.Replicas)
	}

	if memcached.Spec.UpdateStrategy.Type == "" {
		return fmt.Errorf(`'spec.updateStrategy.type' is missing`)
	}

	if memcached.Spec.TerminationPolicy == "" {
		return fmt.Errorf(`'spec.terminationPolicy' is missing`)
	}

	monitorSpec := memcached.Spec.Monitor
	if monitorSpec != nil {
		if err := amv.ValidateMonitorSpec(monitorSpec); err != nil {
			return err
		}
	}

	if err := matchWithDormantDatabase(extClient, memcached); err != nil {
		return err
	}
	return nil
}

func matchWithDormantDatabase(extClient cs.Interface, memcached *api.Memcached) error {
	// Check if DormantDatabase exists or not
	dormantDb, err := extClient.KubedbV1alpha1().DormantDatabases(memcached.Namespace).Get(memcached.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Check DatabaseKind
	if value, _ := meta_util.GetStringValue(dormantDb.Labels, api.LabelDatabaseKind); value != api.ResourceKindMemcached {
		return errors.New(fmt.Sprintf(`invalid Memcached: "%v/%v". Exists DormantDatabase "%v/%v" of different Kind`, memcached.Namespace, memcached.Name, dormantDb.Namespace, dormantDb.Name))
	}

	// Check Origin Spec
	drmnOriginSpec := dormantDb.Spec.Origin.Spec.Memcached
	drmnOriginSpec.SetDefaults()
	originalSpec := memcached.Spec

	// Skip checking UpdateStrategy
	drmnOriginSpec.UpdateStrategy = originalSpec.UpdateStrategy

	// Skip checking TerminationPolicy
	drmnOriginSpec.TerminationPolicy = originalSpec.TerminationPolicy

	// Skip checking Monitoring
	drmnOriginSpec.Monitor = originalSpec.Monitor

	if !meta_util.Equal(drmnOriginSpec, &originalSpec) {
		diff := meta_util.Diff(drmnOriginSpec, &originalSpec)
		log.Errorf("memcached spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff)
		return errors.New(fmt.Sprintf("memcached spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff))
	}

	return nil
}

func validateUpdate(obj, oldObj runtime.Object, kind string) error {
	preconditions := getPreconditionFunc()
	_, err := meta_util.CreateStrategicPatch(oldObj, obj, preconditions...)
	if err != nil {
		if mergepatch.IsPreconditionFailed(err) {
			return fmt.Errorf("%v.%v", err, preconditionFailedError(kind))
		}
		return err
	}
	return nil
}

func getPreconditionFunc() []mergepatch.PreconditionFunc {
	preconditions := []mergepatch.PreconditionFunc{
		mergepatch.RequireKeyUnchanged("apiVersion"),
		mergepatch.RequireKeyUnchanged("kind"),
		mergepatch.RequireMetadataKeyUnchanged("name"),
		mergepatch.RequireMetadataKeyUnchanged("namespace"),
	}

	for _, field := range preconditionSpecFields {
		preconditions = append(preconditions,
			meta_util.RequireChainKeyUnchanged(field),
		)
	}
	return preconditions
}

var preconditionSpecFields = []string{
	"spec.podTemplate.spec.nodeSelector",
}

func preconditionFailedError(kind string) error {
	str := preconditionSpecFields
	strList := strings.Join(str, "\n\t")
	return fmt.Errorf(strings.Join([]string{`At least one of the following was changed:
	apiVersion
	kind
	name
	namespace`, strList}, "\n\t"))
}
