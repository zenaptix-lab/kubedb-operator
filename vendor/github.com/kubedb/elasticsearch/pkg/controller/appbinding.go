package controller

import (
	"github.com/appscode/kutil"
	core_util "github.com/appscode/kutil/core/v1"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	"github.com/kubedb/apimachinery/pkg/eventer"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
	appcat "kmodules.xyz/custom-resources/apis/appcatalog/v1alpha1"
	appcat_util "kmodules.xyz/custom-resources/client/clientset/versioned/typed/appcatalog/v1alpha1/util"
)

func (c *Controller) ensureAppBinding(db *api.Elasticsearch) (kutil.VerbType, error) {
	appmeta := db.AppBindingMeta()

	meta := metav1.ObjectMeta{
		Name:      appmeta.Name(),
		Namespace: db.Namespace,
	}

	ref, err := reference.GetReference(clientsetscheme.Scheme, db)
	if err != nil {
		return kutil.VerbUnchanged, err
	}

	var caBundle []byte
	if db.Spec.EnableSSL {
		certSecret, err := c.Client.CoreV1().Secrets(db.Namespace).Get(db.Spec.CertificateSecret.SecretName, metav1.GetOptions{})
		if err != nil {
			return kutil.VerbUnchanged, errors.Wrapf(err, "failed to read certificate secret for Elasticsearch %s/%s", db.Namespace, db.Name)
		}
		if v, ok := certSecret.Data["root.pem"]; !ok {
			return kutil.VerbUnchanged, errors.Errorf("root.pem is missing in certificate secret for Elasticsearch %s/%s", db.Namespace, db.Name)
		} else {
			caBundle = v
		}
	}

	_, vt, err := appcat_util.CreateOrPatchAppBinding(c.AppCatalogClient, meta, func(in *appcat.AppBinding) *appcat.AppBinding {
		core_util.EnsureOwnerReference(&in.ObjectMeta, ref)
		in.Labels = db.OffshootLabels()
		in.Annotations = db.Spec.ServiceTemplate.Annotations

		in.Spec.Type = appmeta.Type()
		in.Spec.ClientConfig.Service = &appcat.ServiceReference{
			Scheme: db.GetConnectionScheme(),
			Name:   db.ServiceName(),
			Port:   defaultClientPort.Port,
		}
		in.Spec.ClientConfig.CABundle = caBundle
		in.Spec.ClientConfig.InsecureSkipTLSVerify = false

		in.Spec.Secret = &core.LocalObjectReference{
			Name: db.Spec.DatabaseSecret.SecretName,
		}
		in.Spec.SecretTransforms = []appcat.SecretTransform{
			{
				RenameKey: &appcat.RenameKeyTransform{
					From: KeyAdminUserName,
					To:   appcat.KeyUsername,
				},
			},
			{
				RenameKey: &appcat.RenameKeyTransform{
					From: KeyAdminPassword,
					To:   appcat.KeyPassword,
				},
			},
		}

		return in
	})

	if err != nil {
		return kutil.VerbUnchanged, err
	} else if vt != kutil.VerbUnchanged {
		c.recorder.Eventf(
			db,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully %s appbinding",
			vt,
		)
	}
	return vt, nil
}
