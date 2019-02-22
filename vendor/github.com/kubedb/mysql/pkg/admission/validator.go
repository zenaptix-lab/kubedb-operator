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

type MySQLValidator struct {
	client      kubernetes.Interface
	extClient   cs.Interface
	lock        sync.RWMutex
	initialized bool
}

var _ hookapi.AdmissionHook = &MySQLValidator{}

var forbiddenEnvVars = []string{
	"MYSQL_ROOT_PASSWORD",
	"MYSQL_ALLOW_EMPTY_PASSWORD",
	"MYSQL_RANDOM_ROOT_PASSWORD",
	"MYSQL_ONETIME_PASSWORD",
}

func (a *MySQLValidator) Resource() (plural schema.GroupVersionResource, singular string) {
	return schema.GroupVersionResource{
			Group:    "validators.kubedb.com",
			Version:  "v1alpha1",
			Resource: "mysqls",
		},
		"mysql"
}

func (a *MySQLValidator) Initialize(config *rest.Config, stopCh <-chan struct{}) error {
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

func (a *MySQLValidator) Admit(req *admission.AdmissionRequest) *admission.AdmissionResponse {
	status := &admission.AdmissionResponse{}

	if (req.Operation != admission.Create && req.Operation != admission.Update && req.Operation != admission.Delete) ||
		len(req.SubResource) != 0 ||
		req.Kind.Group != api.SchemeGroupVersion.Group ||
		req.Kind.Kind != api.ResourceKindMySQL {
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
			obj, err := a.extClient.KubedbV1alpha1().MySQLs(req.Namespace).Get(req.Name, metav1.GetOptions{})
			if err != nil && !kerr.IsNotFound(err) {
				return hookapi.StatusInternalServerError(err)
			} else if err == nil && obj.Spec.TerminationPolicy == api.TerminationPolicyDoNotTerminate {
				return hookapi.StatusBadRequest(fmt.Errorf(`mysql "%v/%v" can't be paused. To delete, change spec.terminationPolicy`, req.Namespace, req.Name))
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

			mysql := obj.(*api.MySQL).DeepCopy()
			oldMySQL := oldObject.(*api.MySQL).DeepCopy()
			oldMySQL.SetDefaults()
			// Allow changing Database Secret only if there was no secret have set up yet.
			if oldMySQL.Spec.DatabaseSecret == nil {
				oldMySQL.Spec.DatabaseSecret = mysql.Spec.DatabaseSecret
			}

			if err := validateUpdate(mysql, oldMySQL, req.Kind.Kind); err != nil {
				return hookapi.StatusBadRequest(fmt.Errorf("%v", err))
			}
		}
		// validate database specs
		if err = ValidateMySQL(a.client, a.extClient, obj.(*api.MySQL), false); err != nil {
			return hookapi.StatusForbidden(err)
		}
	}
	status.Allowed = true
	return status
}

// ValidateMySQL checks if the object satisfies all the requirements.
// It is not method of Interface, because it is referenced from controller package too.
func ValidateMySQL(client kubernetes.Interface, extClient cs.Interface, mysql *api.MySQL, strictValidation bool) error {
	if mysql.Spec.Version == "" {
		return errors.New(`'spec.version' is missing`)
	}
	if _, err := extClient.CatalogV1alpha1().MySQLVersions().Get(string(mysql.Spec.Version), metav1.GetOptions{}); err != nil {
		return err
	}

	if mysql.Spec.Replicas == nil || *mysql.Spec.Replicas != 1 {
		return fmt.Errorf(`spec.replicas "%v" invalid. Value must be one`, mysql.Spec.Replicas)
	}

	if err := amv.ValidateEnvVar(mysql.Spec.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMySQL); err != nil {
		return err
	}

	if mysql.Spec.StorageType == "" {
		return fmt.Errorf(`'spec.storageType' is missing`)
	}
	if mysql.Spec.StorageType != api.StorageTypeDurable && mysql.Spec.StorageType != api.StorageTypeEphemeral {
		return fmt.Errorf(`'spec.storageType' %s is invalid`, mysql.Spec.StorageType)
	}
	if err := amv.ValidateStorage(client, mysql.Spec.StorageType, mysql.Spec.Storage); err != nil {
		return err
	}

	databaseSecret := mysql.Spec.DatabaseSecret

	if strictValidation {
		if databaseSecret != nil {
			if _, err := client.CoreV1().Secrets(mysql.Namespace).Get(databaseSecret.SecretName, metav1.GetOptions{}); err != nil {
				return err
			}
		}

		// Check if mysqlVersion is deprecated.
		// If deprecated, return error
		mysqlVersion, err := extClient.CatalogV1alpha1().MySQLVersions().Get(string(mysql.Spec.Version), metav1.GetOptions{})
		if err != nil {
			return err
		}

		if mysqlVersion.Spec.Deprecated {
			return fmt.Errorf("mysql %s/%s is using deprecated version %v. Skipped processing", mysql.Namespace, mysql.Name, mysqlVersion.Name)
		}
	}

	if mysql.Spec.Init != nil &&
		mysql.Spec.Init.SnapshotSource != nil &&
		databaseSecret == nil {
		return fmt.Errorf("for Snapshot init, 'spec.databaseSecret.secretName' of %v/%v needs to be similar to older database of snapshot %v/%v",
			mysql.Namespace, mysql.Name, mysql.Spec.Init.SnapshotSource.Namespace, mysql.Spec.Init.SnapshotSource.Name)
	}

	backupScheduleSpec := mysql.Spec.BackupSchedule
	if backupScheduleSpec != nil {
		if err := amv.ValidateBackupSchedule(client, backupScheduleSpec, mysql.Namespace); err != nil {
			return err
		}
	}

	if mysql.Spec.UpdateStrategy.Type == "" {
		return fmt.Errorf(`'spec.updateStrategy.type' is missing`)
	}

	if mysql.Spec.TerminationPolicy == "" {
		return fmt.Errorf(`'spec.terminationPolicy' is missing`)
	}

	if mysql.Spec.StorageType == api.StorageTypeEphemeral && mysql.Spec.TerminationPolicy == api.TerminationPolicyPause {
		return fmt.Errorf(`'spec.terminationPolicy: Pause' can not be used for 'Ephemeral' storage`)
	}

	monitorSpec := mysql.Spec.Monitor
	if monitorSpec != nil {
		if err := amv.ValidateMonitorSpec(monitorSpec); err != nil {
			return err
		}
	}

	if err := matchWithDormantDatabase(extClient, mysql); err != nil {
		return err
	}
	return nil
}

func matchWithDormantDatabase(extClient cs.Interface, mysql *api.MySQL) error {
	// Check if DormantDatabase exists or not
	dormantDb, err := extClient.KubedbV1alpha1().DormantDatabases(mysql.Namespace).Get(mysql.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Check DatabaseKind
	if value, _ := meta_util.GetStringValue(dormantDb.Labels, api.LabelDatabaseKind); value != api.ResourceKindMySQL {
		return errors.New(fmt.Sprintf(`invalid MySQL: "%v/%v". Exists DormantDatabase "%v/%v" of different Kind`, mysql.Namespace, mysql.Name, dormantDb.Namespace, dormantDb.Name))
	}

	// Check Origin Spec
	drmnOriginSpec := dormantDb.Spec.Origin.Spec.MySQL
	drmnOriginSpec.SetDefaults()
	originalSpec := mysql.Spec

	// Skip checking UpdateStrategy
	drmnOriginSpec.UpdateStrategy = originalSpec.UpdateStrategy

	// Skip checking TerminationPolicy
	drmnOriginSpec.TerminationPolicy = originalSpec.TerminationPolicy

	// Skip checking Monitoring
	drmnOriginSpec.Monitor = originalSpec.Monitor

	// Skip Checking Backup Scheduler
	drmnOriginSpec.BackupSchedule = originalSpec.BackupSchedule

	if !meta_util.Equal(drmnOriginSpec, &originalSpec) {
		diff := meta_util.Diff(drmnOriginSpec, &originalSpec)
		log.Errorf("mysql spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff)
		return errors.New(fmt.Sprintf("mysql spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff))
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
	"spec.storageType",
	"spec.storage",
	"spec.databaseSecret",
	"spec.init",
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
