package multiclusterhub

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	storv1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/fatih/structs"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/apis/operators/v1alpha1"
	operatorsv1alpha1 "github.com/open-cluster-management/multicloudhub-operator/pkg/apis/operators/v1alpha1"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/deploying"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/rendering"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/subscription"
	"github.com/open-cluster-management/multicloudhub-operator/pkg/utils"
)

var log = logf.Log.WithName("controller_multiclusterhub")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new MultiClusterHub Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileMultiClusterHub{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("multiclusterhub-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource MultiClusterHub
	err = c.Watch(&source.Kind{Type: &operatorsv1alpha1.MultiClusterHub{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Pods and requeue the owner MultiClusterHub
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &operatorsv1alpha1.MultiClusterHub{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileMultiClusterHub implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileMultiClusterHub{}

// ReconcileMultiClusterHub reconciles a MultiClusterHub object
type ReconcileMultiClusterHub struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	CacheSpec utils.CacheSpec
	scheme    *runtime.Scheme
}

// Reconcile reads that state of the cluster for a MultiClusterHub object and makes changes based on the state read
// and what is in the MultiClusterHub.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileMultiClusterHub) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling MultiClusterHub")

	// Fetch the MultiClusterHub instance
	multiClusterHub := &operatorsv1alpha1.MultiClusterHub{}
	err := r.client.Get(context.TODO(), request.NamespacedName, multiClusterHub)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("MultiClusterHub resource not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get MultiClusterHub CR")
		return reconcile.Result{}, err
	}

	var result *reconcile.Result
	if !utils.MchIsValid(multiClusterHub) {
		log.Info("MultiClusterHub is Invalid. Updating with proper defaults")
		result, err = r.SetDefaults(multiClusterHub)
		if result != nil {
			return *result, err
		}
		log.Info("MultiClusterHub successfully updated")
		// return reconcile.Result{}, nil
	}
	result, err = r.ensureDeployment(multiClusterHub, r.helmRepoDeployment(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureService(multiClusterHub, r.repoService(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureChannel(multiClusterHub, r.helmChannel(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureSubscription(multiClusterHub, subscription.CertManager(multiClusterHub))
	if result != nil {
		return *result, err
	}

	certGV := schema.GroupVersion{Group: "certmanager.k8s.io", Version: "v1alpha1"}
	result, err = r.apiReady(certGV)
	if result != nil {
		return *result, err
	}

	result, err = r.ensureSubscription(multiClusterHub, subscription.CertWebhook(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureSubscription(multiClusterHub, subscription.ConfigWatcher(multiClusterHub))
	if result != nil {
		return *result, err
	}

	result, err = r.ensureSecret(multiClusterHub, r.mongoAuthSecret(multiClusterHub))
	if result != nil {
		return *result, err
	}

	if r.CacheSpec.IngressDomain == "" {
		result, err = r.ingressDomain(multiClusterHub)
		if result != nil {
			return *result, err
		}
	}

	result, err = r.ingressDomain(multiClusterHub)
	if result != nil {
		return *result, err
	}

	//Render the templates with a specified CR
	renderer := rendering.NewRenderer(multiClusterHub, r.CacheSpec)
	toDeploy, err := renderer.Render(r.client)
	if err != nil {
		reqLogger.Error(err, "Failed to render MultiClusterHub templates")
		return reconcile.Result{}, err
	}
	//Deploy the resources
	for _, res := range toDeploy {
		if res.GetNamespace() == multiClusterHub.Namespace {
			if err := controllerutil.SetControllerReference(multiClusterHub, res, r.scheme); err != nil {
				reqLogger.Error(err, "Failed to set controller reference")
			}
		}
		if err := deploying.Deploy(r.client, res); err != nil {
			reqLogger.Error(err, fmt.Sprintf("Failed to deploy %s %s/%s", res.GetKind(), multiClusterHub.Namespace, res.GetName()))
			return reconcile.Result{}, err
		}
	}

	// Update the CR status
	multiClusterHub.Status.Phase = "Pending"
	ready, deployments, err := deploying.ListDeployments(r.client, multiClusterHub.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ready {
		multiClusterHub.Status.Phase = "Running"
	}
	statedDeployments := []operatorsv1alpha1.DeploymentResult{}
	for _, deploy := range deployments {
		statedDeployments = append(statedDeployments, operatorsv1alpha1.DeploymentResult{
			Name:   deploy.Name,
			Status: deploy.Status,
		})
	}
	multiClusterHub.Status.Deployments = statedDeployments

	err = r.client.Status().Update(context.TODO(), multiClusterHub)
	if err != nil {
		if errors.IsConflict(err) {
			// Error from object being modified is normal behavior and should not be treated like an error
			reqLogger.Info("Failed to update status", "Reason", "Object has been modified")
			return reconcile.Result{Requeue: true}, nil
		}

		reqLogger.Error(err, fmt.Sprintf("Failed to update %s/%s status ", multiClusterHub.Namespace, multiClusterHub.Name))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileMultiClusterHub) mongoAuthSecret(v *operatorsv1alpha1.MultiClusterHub) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mongodb-admin",
			Namespace: v.Namespace,
		},
		Type: "Opaque",
		StringData: map[string]string{
			"user":     "some@example.com",
			"password": generatePass(16),
		},
	}

	if err := controllerutil.SetControllerReference(v, secret, r.scheme); err != nil {
		log.Error(err, "Failed to set controller reference", "Secret.Namespace", v.Namespace, "Secret.Name", v.Name)
	}
	return secret
}

func generatePass(length int) string {
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"0123456789"

	buf := make([]byte, length)
	for i := 0; i < length; i++ {
		nBig, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		buf[i] = chars[nBig.Int64()]
	}
	return string(buf)
}

// SetDefaults Updates MultiClusterHub resource with proper defaults
func (r *ReconcileMultiClusterHub) SetDefaults(m *operatorsv1alpha1.MultiClusterHub) (*reconcile.Result, error) {
	if m.Spec.Version == "" {
		m.Spec.Version = utils.LatestVerison
	}

	if m.Spec.ImageRepository == "" {
		m.Spec.ImageRepository = utils.DefaultRepository
	}

	if m.Spec.ImagePullPolicy == "" {
		m.Spec.ImagePullPolicy = corev1.PullAlways
	}

	if m.Spec.Mongo.Storage == "" {
		m.Spec.Mongo.Storage = "1Gi"
	}

	if m.Spec.Mongo.StorageClass == "" {
		storageClass, err := r.getStorageClass()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Mongo.StorageClass = storageClass
	}

	if m.Spec.Etcd.Storage == "" {
		m.Spec.Etcd.Storage = "1Gi"
	}

	if m.Spec.Etcd.StorageClass == "" {
		storageClass, err := r.getStorageClass()
		if err != nil {
			return &reconcile.Result{}, err
		}
		m.Spec.Etcd.StorageClass = storageClass
	}

	if reflect.DeepEqual(structs.Map(m.Spec.Hive), structs.Map(v1alpha1.HiveConfigSpec{})) {
		m.Spec.Hive = v1alpha1.HiveConfigSpec{
			AdditionalCertificateAuthorities: []corev1.LocalObjectReference{
				corev1.LocalObjectReference{
					Name: "letsencrypt-ca",
				},
			},
			FailedProvisionConfig: v1alpha1.FailedProvisionConfig{
				SkipGatherLogs: true,
			},
			GlobalPullSecret: &corev1.LocalObjectReference{
				Name: "private-secret",
			},
		}
	}
	return nil, nil
}

// getStorageClass retrieves the default storage class if it exists
func (r *ReconcileMultiClusterHub) getStorageClass() (string, error) {
	scList := &storv1.StorageClassList{}
	if err := r.client.List(context.TODO(), scList); err != nil {
		return "", err
	}
	for _, sc := range scList.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc.GetName(), nil
		}
	}
	return "", fmt.Errorf("failed to find default storageclass")
}

// ingressDomain is discovered from Openshift cluster configuration resources
func (r *ReconcileMultiClusterHub) ingressDomain(m *operatorsv1alpha1.MultiClusterHub) (*reconcile.Result, error) {
	if r.CacheSpec.IngressDomain != "" {
		return nil, nil
	}

	// Create dynamic client
	dc, err := createDynamicClient()
	if err != nil {
		log.Error(err, "Failed to create dynamic client")
		return &reconcile.Result{}, err
	}

	// Find resource
	schema := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "ingresses"}
	crd, err := dc.Resource(schema).Get("cluster", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "Failed to get resource", "resource", schema.GroupResource().String())
		return &reconcile.Result{}, err
	}

	// Parse resource for domain value
	domain, ok, err := unstructured.NestedString(crd.UnstructuredContent(), "spec", "domain")
	if err != nil {
		log.Error(err, "Error parsing resource", "resource", schema.GroupResource().String(), "value", "spec.domain")
		return &reconcile.Result{}, err
	}
	if !ok {
		err = fmt.Errorf("field not found")
		log.Error(err, "Ingress config did not contain expected value", "resource", schema.GroupResource().String(), "value", "spec.domain")
		return &reconcile.Result{}, err
	}

	log.Info("Ingress domain not set, updating value in cachespec", "MultiClusterHub.Namespace", m.Namespace, "MultiClusterHub.Name", m.Name, "ingressDomain", domain)
	r.CacheSpec.IngressDomain = domain
	err = r.client.Update(context.TODO(), m)
	if err != nil {
		log.Error(err, "Failed to update MultiClusterHub", "MultiClusterHub.Namespace", m.Namespace, "MultiClusterHub.Name", m.Name)
		return &reconcile.Result{}, err
	}

	return nil, nil
}
