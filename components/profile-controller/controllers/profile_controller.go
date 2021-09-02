/*
Copyright 2019 The Kubeflow Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"
	"encoding/json"

	"github.com/ghodss/yaml"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cenkalti/backoff"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	profilev1 "github.com/kubeflow/kubeflow/components/profile-controller/api/v1"
	istioSecurity "istio.io/api/security/v1beta1"
	istioSecurityClient "istio.io/client-go/pkg/apis/security/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// These are imports for creating the scc needed for istio sidecar and knative on OCP 4.7
	securityv1 "github.com/openshift/api/security/v1"
	securityclientv1 "github.com/openshift/client-go/security/clientset/versioned/typed/security/v1"
	networkattachv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	networkattachclientv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	rest "k8s.io/client-go/rest"
)

const AUTHZPOLICYISTIO = "ns-owner-access-istio"

// Istio constants
const ISTIOALLOWALL = "allow-all"

const KFQUOTA = "kf-resource-quota"
const PROFILEFINALIZER = "profile-finalizer"

// annotation key, consumed by kfam API
const USER = "user"
const ROLE = "role"
const ADMIN = "admin"

// Kubeflow default role names
// TODO: Make kubeflow roles configurable (krishnadurai)
// This will enable customization of roles.
const (
	kubeflowAdmin       = "kubeflow-admin"
	kubeflowEdit        = "kubeflow-edit"
	kubeflowView        = "kubeflow-view"
	istioInjectionLabel = "istio-injection"
)

var kubeflowNamespaceLabels = map[string]string{
	"katib-metricscollector-injection":      "enabled",
	"serving.kubeflow.org/inferenceservice": "enabled",
	"pipelines.kubeflow.org/enabled":        "true",
	"app.kubernetes.io/part-of":             "kubeflow-profile",
}

const DEFAULT_EDITOR = "default-editor"
const DEFAULT_VIEWER = "default-viewer"

type Plugin interface {
	// Called when profile CR is created / updated
	ApplyPlugin(*ProfileReconciler, *profilev1.Profile) error
	// Called when profile CR is being deleted, to cleanup any non-k8s resources created via ApplyPlugin
	// RevokePlugin logic need to be IDEMPOTENT
	RevokePlugin(*ProfileReconciler, *profilev1.Profile) error
}

// ProfileReconciler reconciles a Profile object
type ProfileReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Log              logr.Logger
	UserIdHeader     string
	UserIdPrefix     string
	WorkloadIdentity string
}

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs="*"
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs="*"
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs="*"
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs="*"
// +kubebuilder:rbac:groups=kubeflow.org,resources=profiles;profiles/status;profiles/finalizers,verbs="*"

// Reconcile reads that state of the cluster for a Profile object and makes changes based on the state read
// and what is in the Profile.Spec
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
func (r *ProfileReconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	logger := r.Log.WithValues("profile", request.NamespacedName)

	// Fetch the Profile instance
	instance := &profilev1.Profile{}
	logger.Info("Start to Reconcile.", "namespace", request.Namespace, "name", request.Name)
	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			IncRequestCounter("profile deletion")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		IncRequestErrorCounter("error reading the profile object", SEVERITY_MAJOR)
		logger.Error(err, "error reading the profile object")
		return reconcile.Result{}, err
	}

	// Update namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"owner": instance.Spec.Owner.Name},
			// inject istio sidecar to all pods in target namespace by default.
			//Labels: map[string]string{
			//	istioInjectionLabel: "enabled",
			//},
            //Disabling istio injection for now, need to fix the istio sidecar injection of fsgrp 1377
			// This needs to be enabled when integrating with an authentication tool. 
            Labels: map[string]string{},
			Name: instance.Name,
		},
	}
	updateNamespaceLabels(ns)
	if err := controllerutil.SetControllerReference(instance, ns, r.Scheme); err != nil {
		IncRequestErrorCounter("error setting ControllerReference", SEVERITY_MAJOR)
		logger.Error(err, "error setting ControllerReference")
		return reconcile.Result{}, err
	}
	foundNs := &corev1.Namespace{}
	err = r.Get(ctx, types.NamespacedName{Name: ns.Name}, foundNs)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating Namespace: " + ns.Name)
			err = r.Create(ctx, ns)
			if err != nil {
				IncRequestErrorCounter("error creating namespace", SEVERITY_MAJOR)
				logger.Error(err, "error creating namespace")
				return reconcile.Result{}, err
			}
			// wait 15 seconds for new namespace creation.
			err = backoff.Retry(
				func() error {
					return r.Get(ctx, types.NamespacedName{Name: ns.Name}, foundNs)
				},
				backoff.WithMaxRetries(backoff.NewConstantBackOff(3*time.Second), 5))
			if err != nil {
				IncRequestErrorCounter("error namespace create completion", SEVERITY_MAJOR)
				logger.Error(err, "error namespace create completion")
				return r.appendErrorConditionAndReturn(ctx, instance,
					"Owning namespace failed to create within 15 seconds")
			}
			logger.Info("Created Namespace: "+foundNs.Name, "status", foundNs.Status.Phase)
		} else {
			IncRequestErrorCounter("error reading namespace", SEVERITY_MAJOR)
			logger.Error(err, "error reading namespace")
			return reconcile.Result{}, err
		}
	} else {
		// Check exising namespace ownership before move forward
		owner, ok := foundNs.Annotations["owner"]
		if ok && owner == instance.Spec.Owner.Name {
			if updated := updateNamespaceLabels(foundNs); updated {
				err = r.Update(ctx, foundNs)
				if err != nil {
					IncRequestErrorCounter("error updating namespace label", SEVERITY_MAJOR)
					logger.Error(err, "error updating namespace label")
					return reconcile.Result{}, err
				}
			}
		} else {
			logger.Info(fmt.Sprintf("namespace already exist, but not owned by profile creator %v",
				instance.Spec.Owner.Name))
			IncRequestCounter("reject profile taking over existing namespace")
			return r.appendErrorConditionAndReturn(ctx, instance, fmt.Sprintf(
				"namespace already exist, but not owned by profile creator %v", instance.Spec.Owner.Name))
		}
	}

	// Update anyuid SCC for Openshift installation
	// this allows any pod running in new profile namespaces to run as anyuid for user and group
	// Ths is needed for both Istio side car and Knative in OCP 4.6/4.7
	if err = r.updateAnyUIDScc(ctx, instance); err != nil {
		logger.Error(err, "error updating anyuid SCC", "namespace", instance.Name)
		IncRequestErrorCounter("error updating anyuid SCC", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}

	// Update NetworkAttachmentDefinition
	// this is needed for isito on OCP using istio-cni
	// refer to documentation here: https://istio.io/latest/docs/setup/platform-setup/openshift/
	if err = r.updateNetworkAttachmentDefinition(ctx, instance); err != nil {
		logger.Error(err, "error updating NetworkAttachmentDefinition", "namespace", instance.Name)
		IncRequestErrorCounter("error updating NetworkAttachmentDefinition", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}

	// Update Istio AuthorizationPolicy
	// Create Istio AuthorizationPolicy in target namespace, which will give ns owner permission to access services in ns.
	if err = r.updateIstioAuthorizationPolicy(instance); err != nil {
		logger.Error(err, "error Updating Istio AuthorizationPolicy permission", "namespace", instance.Name)
		IncRequestErrorCounter("error updating Istio AuthorizationPolicy permission", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}

	// Update service accounts
	// Create service account "default-editor" in target namespace.
	// "default-editor" would have kubeflowEdit permission: edit all resources in target namespace except rbac.
	if err = r.updateServiceAccount(instance, DEFAULT_EDITOR, kubeflowEdit); err != nil {
		logger.Error(err, "error Updating ServiceAccount", "namespace", instance.Name, "name",
			"defaultEditor")
		IncRequestErrorCounter("error updating ServiceAccount", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}
	// Create service account "default-viewer" in target namespace.
	// "default-viewer" would have k8s default "view" permission: view all resources in target namespace.
	if err = r.updateServiceAccount(instance, DEFAULT_VIEWER, kubeflowView); err != nil {
		logger.Error(err, "error Updating ServiceAccount", "namespace", instance.Name, "name",
			"defaultViewer")
		IncRequestErrorCounter("error updating ServiceAccount", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}

	// TODO: add role for impersonate permission

	// Update owner rbac permission
	// When ClusterRole was referred by namespaced roleBinding, the result permission will be namespaced as well.
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{USER: instance.Spec.Owner.Name, ROLE: ADMIN},
			Name:        "namespaceAdmin",
			Namespace:   instance.Name,
		},
		// Use default ClusterRole 'admin' for profile/namespace owner
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     kubeflowAdmin,
		},
		Subjects: []rbacv1.Subject{
			instance.Spec.Owner,
		},
	}
	if err = r.updateRoleBinding(instance, roleBinding); err != nil {
		logger.Error(err, "error Updating Owner Rolebinding", "namespace", instance.Name, "name",
			"defaultEdittor")
		IncRequestErrorCounter("error updating Owner Rolebinding", SEVERITY_MAJOR)
		return reconcile.Result{}, err
	}
	// Create resource quota for target namespace if resources are specified in profile.
	if len(instance.Spec.ResourceQuotaSpec.Hard) > 0 {
		resourceQuota := &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      KFQUOTA,
				Namespace: instance.Name,
			},
			Spec: instance.Spec.ResourceQuotaSpec,
		}
		if err = r.updateResourceQuota(instance, resourceQuota); err != nil {
			logger.Error(err, "error Updating resource quota", "namespace", instance.Name)
			IncRequestErrorCounter("error updating resource quota", SEVERITY_MAJOR)
			return reconcile.Result{}, err
		}
	} else {
		logger.Info("No update on resource quota", "spec", instance.Spec.ResourceQuotaSpec.String())
	}
	if err := r.PatchDefaultPluginSpec(ctx, instance); err != nil {
		IncRequestErrorCounter("error patching DefaultPluginSpec", SEVERITY_MAJOR)
		logger.Error(err, "Failed patching DefaultPluginSpec", "namespace", instance.Name)
		return reconcile.Result{}, err
	}
	if plugins, err := r.GetPluginSpec(instance); err == nil {
		for _, plugin := range plugins {
			if err2 := plugin.ApplyPlugin(r, instance); err2 != nil {
				logger.Error(err2, "Failed applying plugin", "namespace", instance.Name)
				IncRequestErrorCounter("error applying plugin", SEVERITY_MAJOR)
				return reconcile.Result{}, err2
			}
		}
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(instance.ObjectMeta.Finalizers, PROFILEFINALIZER) {
			instance.ObjectMeta.Finalizers = append(instance.ObjectMeta.Finalizers, PROFILEFINALIZER)
			if err := r.Update(ctx, instance); err != nil {
				logger.Error(err, "error updating finalizer", "namespace", instance.Name)
				IncRequestErrorCounter("error updating finalizer", SEVERITY_MAJOR)
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(instance.ObjectMeta.Finalizers, PROFILEFINALIZER) {
			// our finalizer is present, so lets revoke all Plugins to clean up any external dependencies
			if plugins, err := r.GetPluginSpec(instance); err == nil {
				for _, plugin := range plugins {
					if err := plugin.RevokePlugin(r, instance); err != nil {
						logger.Error(err, "error revoking plugin", "namespace", instance.Name)
						IncRequestErrorCounter("error revoking plugin", SEVERITY_MAJOR)
						return reconcile.Result{}, err
					}
				}
			}

			// remove our finalizer from the list and update it.
			instance.ObjectMeta.Finalizers = removeString(instance.ObjectMeta.Finalizers, PROFILEFINALIZER)
			if err := r.Update(ctx, instance); err != nil {
				logger.Error(err, "error removing finalizer", "namespace", instance.Name)
				IncRequestErrorCounter("error removing finalizer", SEVERITY_MAJOR)
				return ctrl.Result{}, err
			}
		}
	}
	IncRequestCounter("reconcile")
	return ctrl.Result{}, nil
}

// appendErrorConditionAndReturn append failure status to profile CR and mark Reconcile done. If update condition failed, request will be requeued.
func (r *ProfileReconciler) appendErrorConditionAndReturn(ctx context.Context, instance *profilev1.Profile,
	message string) (ctrl.Result, error) {
	instance.Status.Conditions = append(instance.Status.Conditions, profilev1.ProfileCondition{
		Type:    profilev1.ProfileFailed,
		Message: message,
	})
	if err := r.Update(ctx, instance); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *ProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//if err := securityv1.AddToScheme(mgr.GetScheme()); err != nil {
	//	return err
	//}
	//if err := networkattachv1.AddToScheme(mgr.GetScheme()); err != nil {
	//	return err
	//}
	return ctrl.NewControllerManagedBy(mgr).
		For(&profilev1.Profile{}).
		Owns(&corev1.Namespace{}).
		Owns(&istioSecurityClient.AuthorizationPolicy{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&securityv1.SecurityContextConstraints{}).
		Owns(&networkattachv1.NetworkAttachmentDefinition{}).
		Complete(r)
}

func (r *ProfileReconciler) getAuthorizationPolicy(profileIns *profilev1.Profile) istioSecurity.AuthorizationPolicy {
	return istioSecurity.AuthorizationPolicy{
		Action: istioSecurity.AuthorizationPolicy_ALLOW,
		// Empty selector == match all workloads in namespace
		Selector: nil,
		Rules: []*istioSecurity.Rule{
			{
				When: []*istioSecurity.Condition{
					{
						// Namespace Owner can access all workloads in the
						// namespace
						Key: fmt.Sprintf("request.headers[%v]", r.UserIdHeader),
						Values: []string{
							r.UserIdPrefix + profileIns.Spec.Owner.Name,
						},
					},
				},
			},
			{
				When: []*istioSecurity.Condition{
					{
						// Workloads in the same namespace can access all other
						// workloads in the namespace
						Key:    fmt.Sprintf("source.namespace"),
						Values: []string{profileIns.Name},
					},
				},
			},
		},
	}
}


// updateAnyUIDScc Update anyuid SCC for Openshift installation
// this allows any pod running in new profile namespaces to run as anyuid for user and group
// Ths is needed for both Istio side car and Knative in OCP 4.6/4.7
func (r *ProfileReconciler) updateAnyUIDScc(ctx context.Context, profileIns *profilev1.Profile) error {
	logger := r.Log.WithValues("profile", profileIns.Name)
	logger.Info("Creating anyuid SCC for profile")
	var priority int32 = 10
	scc := &securityv1.SecurityContextConstraints{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security.openshift.io/v1",
			Kind:       "SecurityContextConstraints",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "scc-"+profileIns.Name,
		},
		Priority:                 &priority,
		AllowPrivilegedContainer: true,
		AllowHostNetwork:         false,
		AllowHostPorts:           false,
		//Volumes:                  ["*"],
		SELinuxContext: securityv1.SELinuxContextStrategyOptions{
			Type: securityv1.SELinuxStrategyMustRunAs,
		},
		RunAsUser: securityv1.RunAsUserStrategyOptions{
			Type: securityv1.RunAsUserStrategyRunAsAny,
		},
		FSGroup: securityv1.FSGroupStrategyOptions{
			Type: securityv1.FSGroupStrategyRunAsAny,
		},
		//This needs to be removed, since sa default-editor is attached to notebooks and it fails to use volume when an scc is present
		//Users: []string{"system:serviceaccount:"+profileIns.Name, "system:serviceaccount:"+profileIns.Name+":default-editor"},
		// switched to default that is used by kfserving for serving pods
		Users: []string{"system:serviceaccount:"+profileIns.Name, "system:serviceaccount:"+profileIns.Name+":default"},
		Groups: []string{"system:cluster-admins"},
	}
	if err := controllerutil.SetControllerReference(profileIns, scc, r.Scheme); err != nil {
		return err
	}

	config, err1 := rest.InClusterConfig()
	if err1 != nil{
		logger.Info("Error getting config for K8 client")
		return err1
	}
	securityCLientset, err2 := securityclientv1.NewForConfig(config)
	if err2 != nil{
		logger.Info("Error setting config for K8 client")
		return err2
	}
	// Check if the scc already exists first
	foundscc, err3 := securityCLientset.SecurityContextConstraints().Get(ctx, "scc-"+profileIns.Name, metav1.GetOptions{})
	if errors.IsNotFound(err3) {
		// Create SCC
		actual, err4 := securityCLientset.SecurityContextConstraints().Create(ctx, scc, metav1.CreateOptions{FieldManager:"profile-controller"})
		if err4 != nil {
			logger.Info("Error creating SCC ")
			return err4
		}
		if actual != nil {
	 		logger.Info("Successfully Created SCC")
		}
	} else {
		 logger.Info("SCC already exists " + "scc-"+profileIns.Name)
		 logger.Info("scc found", "found", foundscc)
		 // update SCC
		 if !reflect.DeepEqual(foundscc, scc) {
			sccPatch, err := json.Marshal(scc)
			if err != nil {
				 return err
			}
			var forceApply = true
			updatedscc, err5 := securityCLientset.SecurityContextConstraints().Patch(ctx, foundscc.Name, types.ApplyPatchType, sccPatch, metav1.PatchOptions{FieldManager: "profile-controller", Force: &forceApply})
		    if err5 != nil {
			    logger.Info("Error updating SCC ")
			     return err5
		    }
		    if updatedscc != nil {
	 		    logger.Info("Successfully Updated SCC:", "updated", updatedscc)
		    }
		 }
	} 
	return nil
}

// Update NetworkAttachmentDefinition
// this is needed for isito on OCP using istio-cni
// refer to documentation here: https://istio.io/latest/docs/setup/platform-setup/openshift/
func (r *ProfileReconciler) updateNetworkAttachmentDefinition(ctx context.Context, profileIns *profilev1.Profile) error {
	logger := r.Log.WithValues("profile", profileIns.Name)
	logger.Info("Updating NetworkAttachmentDefinition")
	net := &networkattachv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "istio-cni",
			Namespace: profileIns.Name,
		},
		//Spec: netv1.NetworkAttachmentDefinitionSpec{
		//	Config: config,
		//},
	}
	if err := controllerutil.SetControllerReference(profileIns, net, r.Scheme); err != nil {
		return err
	}

	config, err1 := rest.InClusterConfig()
	if err1 != nil{
		logger.Info("Error getting config for K8 client")
		return err1
	}
	//NewForConfig creates a new K8sCniCncfIoV1Client for the given config.
	networkCLientset, err2 := networkattachclientv1.NewForConfig(config)
	if err2 != nil{
		logger.Info("Error setting config for K8 client")
		return err2
	}
	// Check if the NetworkAttachmentDefinition already exists first
	foundnet, err3 := networkCLientset.NetworkAttachmentDefinitions(profileIns.Name).Get(ctx, "istio-cni", metav1.GetOptions{})
	if errors.IsNotFound(err3) {
		// Create SCC
		actual, err4 := networkCLientset.NetworkAttachmentDefinitions(profileIns.Name).Create(ctx, net, metav1.CreateOptions{})
		if err4 != nil {
			logger.Info("Error creating NetworkAttachmentDefinition ")
			return err4
		}
		if actual != nil {
	 		logger.Info("Successfully Created NetworkAttachmentDefinition")
		}
	} else {
		 logger.Info(" NetworkAttachmentDefinition already exists" + "scc-"+profileIns.Name)
		 logger.Info(" NetworkAttachmentDefinition found", "found", foundnet)
		 // update SCC
		 if !reflect.DeepEqual(foundnet, net) {
			foundnet.Spec= net.Spec
		 	updatedscc, err5 := networkCLientset.NetworkAttachmentDefinitions(profileIns.Name).Update(ctx, foundnet, metav1.UpdateOptions{})
		 	if err5 != nil {
				logger.Info("Error updating NetworkAttachmentDefinition")
				return err5
			}
			if updatedscc != nil {
	 			logger.Info("Successfully Updated NetworkAttachmentDefinition")
			}
		}
	} 
	return nil
}

// updateIstioAuthorizationPolicy create or update Istio AuthorizationPolicy
// resources in target namespace owned by "profileIns". The goal is to allow
// service access for profile owner.
func (r *ProfileReconciler) updateIstioAuthorizationPolicy(profileIns *profilev1.Profile) error {
	logger := r.Log.WithValues("profile", profileIns.Name)

	istioAuth := &istioSecurityClient.AuthorizationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{USER: profileIns.Spec.Owner.Name, ROLE: ADMIN},
			Name:        AUTHZPOLICYISTIO,
			Namespace:   profileIns.Name,
		},
		Spec: r.getAuthorizationPolicy(profileIns),
	}

	if err := controllerutil.SetControllerReference(profileIns, istioAuth, r.Scheme); err != nil {
		return err
	}
	foundAuthorizationPolicy := &istioSecurityClient.AuthorizationPolicy{}
	err := r.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      istioAuth.ObjectMeta.Name,
			Namespace: istioAuth.ObjectMeta.Namespace,
		},
		foundAuthorizationPolicy,
	)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating Istio AuthorizationPolicy", "namespace", istioAuth.ObjectMeta.Namespace,
				"name", istioAuth.ObjectMeta.Name)
			err = r.Create(context.TODO(), istioAuth)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		if !reflect.DeepEqual(istioAuth, foundAuthorizationPolicy) {
			foundAuthorizationPolicy.Spec = istioAuth.Spec
			logger.Info("Updating Istio AuthorizationPolicy", "namespace", istioAuth.ObjectMeta.Namespace,
				"name", istioAuth.ObjectMeta.Name)
			err = r.Update(context.TODO(), foundAuthorizationPolicy)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// updateResourceQuota create or update ResourceQuota for target namespace
func (r *ProfileReconciler) updateResourceQuota(profileIns *profilev1.Profile,
	resourceQuota *corev1.ResourceQuota) error {
	ctx := context.Background()
	logger := r.Log.WithValues("profile", profileIns.Name)
	if err := controllerutil.SetControllerReference(profileIns, resourceQuota, r.Scheme); err != nil {
		return err
	}
	found := &corev1.ResourceQuota{}
	err := r.Get(ctx, types.NamespacedName{Name: resourceQuota.Name, Namespace: resourceQuota.Namespace}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating ResourceQuota", "namespace", resourceQuota.Namespace, "name", resourceQuota.Name)
			err = r.Create(ctx, resourceQuota)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		if !(reflect.DeepEqual(resourceQuota.Spec, found.Spec)) {
			found.Spec = resourceQuota.Spec
			logger.Info("Updating ResourceQuota", "namespace", resourceQuota.Namespace, "name", resourceQuota.Name)
			err = r.Update(ctx, found)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// updateServiceAccount create or update service account "saName" with role "ClusterRoleName" in target namespace owned by "profileIns"
func (r *ProfileReconciler) updateServiceAccount(profileIns *profilev1.Profile, saName string,
	ClusterRoleName string) error {
	logger := r.Log.WithValues("profile", profileIns.Name)
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: profileIns.Name,
		},
	}
	if err := controllerutil.SetControllerReference(profileIns, serviceAccount, r.Scheme); err != nil {
		return err
	}
	found := &corev1.ServiceAccount{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: serviceAccount.Name, Namespace: serviceAccount.Namespace}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating ServiceAccount", "namespace", serviceAccount.Namespace,
				"name", serviceAccount.Name)
			err = r.Create(context.TODO(), serviceAccount)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: profileIns.Name,
		},
		// Use default ClusterRole 'admin' for profile/namespace owner
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     ClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      saName,
				Namespace: profileIns.Name,
			},
		},
	}
	return r.updateRoleBinding(profileIns, roleBinding)
}

// updateRoleBinding create or update roleBinding "roleBinding" in target namespace owned by "profileIns"
func (r *ProfileReconciler) updateRoleBinding(profileIns *profilev1.Profile,
	roleBinding *rbacv1.RoleBinding) error {
	logger := r.Log.WithValues("profile", profileIns.Name)
	if err := controllerutil.SetControllerReference(profileIns, roleBinding, r.Scheme); err != nil {
		return err
	}
	found := &rbacv1.RoleBinding{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: roleBinding.Name, Namespace: roleBinding.Namespace}, found)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating RoleBinding", "namespace", roleBinding.Namespace, "name", roleBinding.Name)
			err = r.Create(context.TODO(), roleBinding)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		if !(reflect.DeepEqual(roleBinding.RoleRef, found.RoleRef) && reflect.DeepEqual(roleBinding.Subjects, found.Subjects)) {
			found.RoleRef = roleBinding.RoleRef
			found.Subjects = roleBinding.Subjects
			logger.Info("Updating RoleBinding", "namespace", roleBinding.Namespace, "name", roleBinding.Name)
			err = r.Update(context.TODO(), found)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// GetPluginSpec will try to unmarshal the plugin spec inside profile for the specified plugin
// Returns an error if the plugin isn't defined or if there is a problem
func (r *ProfileReconciler) GetPluginSpec(profileIns *profilev1.Profile) ([]Plugin, error) {
	logger := r.Log.WithValues("profile", profileIns.Name)
	plugins := []Plugin{}
	for _, p := range profileIns.Spec.Plugins {
		var pluginIns Plugin
		switch p.Kind {
		case KIND_WORKLOAD_IDENTITY:
			pluginIns = &GcpWorkloadIdentity{}
		case KIND_AWS_IAM_FOR_SERVICE_ACCOUNT:
			pluginIns = &AwsIAMForServiceAccount{}
		default:
			logger.Info("Plugin not recgonized: ", "Kind", p.Kind)
			continue
		}

		// To deserialize it to a specific type we need to first serialize it to bytes
		// and then unserialize it.
		specBytes, err := yaml.Marshal(p.Spec)

		if err != nil {
			logger.Info("Could not marshal plugin ", p.Kind, "; error: ", err)
			return nil, err
		}

		err = yaml.Unmarshal(specBytes, pluginIns)
		if err != nil {
			logger.Info("Could not unmarshal plugin ", p.Kind, "; error: ", err)
			return nil, err
		}
		plugins = append(plugins, pluginIns)
	}
	return plugins, nil
}

// PatchDefaultPluginSpec patch default plugins to profile CR instance if user doesn't specify plugin of same kind in CR.
func (r *ProfileReconciler) PatchDefaultPluginSpec(ctx context.Context, profileIns *profilev1.Profile) error {
	// read existing plugins into map
	plugins := make(map[string]profilev1.Plugin)
	for _, p := range profileIns.Spec.Plugins {
		plugins[p.Kind] = p
	}
	// Patch default plugins if same kind doesn't exist yet.
	if r.WorkloadIdentity != "" {
		if _, ok := plugins[KIND_WORKLOAD_IDENTITY]; !ok {
			profileIns.Spec.Plugins = append(profileIns.Spec.Plugins, profilev1.Plugin{
				TypeMeta: metav1.TypeMeta{
					Kind: KIND_WORKLOAD_IDENTITY,
				},
				Spec: &runtime.RawExtension{
					Raw: []byte(fmt.Sprintf(`{"gcpServiceAccount": "%v"}`, r.WorkloadIdentity)),
				},
			})
		}
	}
	if err := r.Update(ctx, profileIns); err != nil {
		return err
	}
	return nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}

func updateNamespaceLabels(ns *corev1.Namespace) bool {
	updated := false
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	for k, v := range kubeflowNamespaceLabels {
		if _, ok := ns.Labels[k]; !ok {
			ns.Labels[k] = v
			updated = true
		}
	}
	return updated
}
