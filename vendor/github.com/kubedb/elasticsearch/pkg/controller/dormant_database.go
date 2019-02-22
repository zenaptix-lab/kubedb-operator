package controller

import (
	core_util "github.com/appscode/kutil/core/v1"
	dynamic_util "github.com/appscode/kutil/dynamic"
	meta_util "github.com/appscode/kutil/meta"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	"github.com/kubedb/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha1/util"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
)

// WaitUntilPaused is an Interface of *amc.Controller
func (c *Controller) WaitUntilPaused(drmn *api.DormantDatabase) error {
	db := &api.Elasticsearch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drmn.OffshootName(),
			Namespace: drmn.Namespace,
		},
	}

	if err := core_util.WaitUntilPodDeletedBySelector(c.Client, db.Namespace, metav1.SetAsLabelSelector(db.OffshootSelectors())); err != nil {
		return err
	}

	if err := core_util.WaitUntilServiceDeletedBySelector(c.Client, db.Namespace, metav1.SetAsLabelSelector(db.OffshootSelectors())); err != nil {
		return err
	}

	if err := c.waitUntilRBACStuffDeleted(db); err != nil {
		return err
	}

	return nil
}

func (c *Controller) waitUntilRBACStuffDeleted(elasticsearch *api.Elasticsearch) error {
	// Delete ServiceAccount
	if err := core_util.WaitUntillServiceAccountDeleted(c.Client, elasticsearch.ObjectMeta); err != nil {
		return err
	}

	// Delete Snapshot ServiceAccount
	snapSAMeta := metav1.ObjectMeta{
		Name:      elasticsearch.SnapshotSAName(),
		Namespace: elasticsearch.Namespace,
	}
	if err := core_util.WaitUntillServiceAccountDeleted(c.Client, snapSAMeta); err != nil {
		return err
	}

	return nil
}

// WipeOutDatabase is an Interface of *amc.Controller.
// It verifies and deletes secrets and other left overs of DBs except Snapshot and PVC.
func (c *Controller) WipeOutDatabase(drmn *api.DormantDatabase) error {
	ref, rerr := reference.GetReference(clientsetscheme.Scheme, drmn)
	if rerr != nil {
		return rerr
	}
	if err := c.wipeOutDatabase(drmn.ObjectMeta, drmn.GetDatabaseSecrets(), ref); err != nil {
		return errors.Wrap(err, "error in wiping out database.")
	}

	return nil
}

// wipeOutDatabase is a generic function to call from WipeOutDatabase and elasticsearch pause method.
func (c *Controller) wipeOutDatabase(meta metav1.ObjectMeta, secrets []string, ref *core.ObjectReference) error {
	secretUsed, err := c.secretsUsedByPeers(meta)
	if err != nil {
		return errors.Wrap(err, "error in getting used secret list")
	}
	unusedSecrets := sets.NewString(secrets...).Difference(secretUsed)
	return dynamic_util.EnsureOwnerReferenceForItems(
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("secrets"),
		meta.Namespace,
		unusedSecrets.List(),
		ref)
}

func (c *Controller) deleteMatchingDormantDatabase(elasticsearch *api.Elasticsearch) error {
	// Check if DormantDatabase exists or not
	ddb, err := c.ExtClient.KubedbV1alpha1().DormantDatabases(elasticsearch.Namespace).Get(elasticsearch.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Set WipeOut to false
	if _, _, err := util.PatchDormantDatabase(c.ExtClient.KubedbV1alpha1(), ddb, func(in *api.DormantDatabase) *api.DormantDatabase {
		in.Spec.WipeOut = false
		return in
	}); err != nil {
		return err
	}

	// Delete  Matching dormantDatabase
	if err := c.ExtClient.KubedbV1alpha1().DormantDatabases(elasticsearch.Namespace).Delete(elasticsearch.Name,
		meta_util.DeleteInBackground()); err != nil && !kerr.IsNotFound(err) {
		return err
	}

	return nil
}

func (c *Controller) createDormantDatabase(elasticsearch *api.Elasticsearch) (*api.DormantDatabase, error) {
	dormantDb := &api.DormantDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elasticsearch.Name,
			Namespace: elasticsearch.Namespace,
			Labels: map[string]string{
				api.LabelDatabaseKind: api.ResourceKindElasticsearch,
			},
		},
		Spec: api.DormantDatabaseSpec{
			Origin: api.Origin{
				ObjectMeta: metav1.ObjectMeta{
					Name:              elasticsearch.Name,
					Namespace:         elasticsearch.Namespace,
					Labels:            elasticsearch.Labels,
					Annotations:       elasticsearch.Annotations,
					CreationTimestamp: elasticsearch.CreationTimestamp,
				},
				Spec: api.OriginSpec{
					Elasticsearch: &(elasticsearch.Spec),
				},
			},
		},
	}

	return c.ExtClient.KubedbV1alpha1().DormantDatabases(dormantDb.Namespace).Create(dormantDb)
}

// isSecretUsed gets the DBList of same kind, then checks if our required secret is used by those.
// Similarly, isSecretUsed also checks for DomantDB of similar dbKind label.
func (c *Controller) secretsUsedByPeers(meta metav1.ObjectMeta) (sets.String, error) {
	secretUsed := sets.NewString()

	dbList, err := c.esLister.Elasticsearches(meta.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, es := range dbList {
		if es.Name != meta.Name {
			secretUsed.Insert(es.Spec.GetSecrets()...)
		}
	}

	labelMap := map[string]string{
		api.LabelDatabaseKind: api.ResourceKindElasticsearch,
	}
	drmnList, err := c.ExtClient.KubedbV1alpha1().DormantDatabases(meta.Namespace).List(
		metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labelMap).String(),
		},
	)
	if err != nil {
		return nil, err
	}
	for _, ddb := range drmnList.Items {
		if ddb.Name != meta.Name {
			secretUsed.Insert(ddb.GetDatabaseSecrets()...)
		}
	}

	return secretUsed, nil
}
