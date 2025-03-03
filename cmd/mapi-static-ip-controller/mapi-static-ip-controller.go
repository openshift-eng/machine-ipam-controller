package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/emicklei/go-restful/v3/log"
	"github.com/go-logr/logr"
	osclientset "github.com/openshift/client-go/config/clientset/versioned"
	mapiclientset "github.com/openshift/client-go/machine/clientset/versioned"
	"github.com/pkg/errors"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamcontrollerv1 "github.com/openshift-splat-team/machine-ipam-controller/pkg/apis/ipamcontroller.openshift.io/v1"
	"github.com/openshift-splat-team/machine-ipam-controller/pkg/mgmt"
)

var (
	mu sync.Mutex
)

func main() {

	// Parse command-line arguments
	flag.Parse()

	logf.SetLogger(zap.New())

	log := logf.Log.WithName("main")

	ctrl.SetLogger(log)

	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{})
	if err != nil {
		log.Error(err, "could not create manager")
		os.Exit(1)
	}
	_, err = osclientset.NewForConfig(config.GetConfigOrDie())
	if err != nil {
		log.Error(err, "could not create osclientset")
		os.Exit(1)
	}

	_, err = mapiclientset.NewForConfig(config.GetConfigOrDie())
	if err != nil {
		log.Error(err, "could not create mapiclientset")
		os.Exit(1)
	}

	// Register object scheme to allow deserialization
	err = ipamv1.AddToScheme(mgr.GetScheme())
	if err != nil {
		log.Error(err, "could not add ipamv1 to scheme")
		os.Exit(1)
	}

	err = ipamcontrollerv1.AddToScheme(mgr.GetScheme())
	if err != nil {
		log.Error(err, "could not add ipamcontrollerv1 to scheme")
		os.Exit(1)
	}

	if err := (&IPPoolClaimProcessor{
		log: log,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "IPPoolClaimController")
		os.Exit(1)
	}

	if err := (&IPPoolController{
		log: log,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller", "controller", "IPPoolController")
		os.Exit(1)
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "could not start manager")
		os.Exit(1)
	}
}

type IPPoolClaimProcessor struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	RESTMapper     meta.RESTMapper
	UncachedClient client.Client
	log            logr.Logger
}

type IPPoolController struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	RESTMapper     meta.RESTMapper
	UncachedClient client.Client
	log            logr.Logger
}

func (a *IPPoolClaimProcessor) BindClaim(ctx context.Context, ipAddressClaim *ipamv1.IPAddressClaim) error {
	a.log.Info("Received BindClaim")
	ip, err := mgmt.GetIPAddress(ctx, ipAddressClaim)
	if err != nil {
		a.log.Error(err, "Unable to get IPAddress")
		return err
	}
	a.log.Info("Got IPAddress %v", ip)

	// create ipaddress object
	if err = a.Client.Create(ctx, ip); err != nil {
		a.log.Error(err, "Unable to create IPAddress")
		err2 := mgmt.ReleaseIPConfiguration(ctx, ip)
		if err2 != nil {
			a.log.Error(err, "Unable to release IPAddress")
			return errors.Wrap(err, "Unable to release IPAddress")
		}
		return err
	}
	ipAddressClaim.Status = ipamv1.IPAddressClaimStatus{
		AddressRef: corev1.LocalObjectReference{
			Name: ip.ObjectMeta.Name,
		},
	}
	if err = a.Client.Status().Update(ctx, ipAddressClaim); err != nil {
		a.log.Error(err, "Unable to update claim")
		return err
	}

	log.Printf("IAC: %v", ipAddressClaim)
	return nil
}

func (a *IPPoolClaimProcessor) ReleaseClaim(ctx context.Context, namespacedName types.NamespacedName) error {
	a.log.Info("Received ReleaseClaim")
	ipAddress := &ipamv1.IPAddress{}
	if err := a.Get(ctx, namespacedName, ipAddress); err != nil {
		return err
	}
	a.log.Info(fmt.Sprintf("Got IPAddress %v (%v)", ipAddress.Name, ipAddress.Spec.Address))
	if err := mgmt.ReleaseIPConfiguration(ctx, ipAddress); err != nil {
		a.log.Error(err, "Unable to release IP")
		return err
	}
	a.log.Info(fmt.Sprintf("Deleting ipaddress CR %v", ipAddress.Name))
	err := a.Delete(ctx, ipAddress)
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (a *IPPoolClaimProcessor) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1.IPAddressClaim{}).
		Complete(a); err != nil {
		return fmt.Errorf("could not set up controller for ip pool claim: %w", err)
	}

	// Set up API helpers from the manager.
	a.Client = mgr.GetClient()
	a.Scheme = mgr.GetScheme()
	a.Recorder = mgr.GetEventRecorderFor("ip-pool-claim-controller")
	a.RESTMapper = mgr.GetRESTMapper()

	return nil
}

func (a *IPPoolClaimProcessor) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	mu.Lock()
	defer mu.Unlock()

	a.log.Info(fmt.Sprintf("Reconciling request %v", req))
	defer a.log.Info(fmt.Sprintf("Finished reconciling request %v", req))

	ipAddressClaim := &ipamv1.IPAddressClaim{}
	claimKey := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}

	if err := a.Get(ctx, claimKey, ipAddressClaim); err != nil {
		a.log.Error(err, "Got error")
		if strings.Contains(fmt.Sprintf("%v", err), "not found") {
			a.log.Info("Handling remove of claim")
			_ = a.ReleaseClaim(ctx, req.NamespacedName)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	a.log.Info(fmt.Sprintf("Got IPAddressClaim %v", ipAddressClaim.Name))

	// Check claim to see if it needs IP from a pool that we own.
	poolRef := ipAddressClaim.Spec.PoolRef
	a.log.V(4).Info(fmt.Sprintf("Kind(%v) Group(%v) Name(%v)", poolRef.Kind, *poolRef.APIGroup, poolRef.Name))

	if poolRef.Kind == ipamcontrollerv1.IPPoolKind && *poolRef.APIGroup == ipamcontrollerv1.APIGroupName {
		a.log.V(4).Info(fmt.Sprintf("Found a claim for an IP from this provider.  Status: %v", ipAddressClaim.Status))
		if ipAddressClaim.Status.AddressRef.Name == "" {
			err := a.BindClaim(ctx, ipAddressClaim)
			if err != nil {
				return reconcile.Result{}, err
			}
		} else {
			// Status was set.  Verify address still exists?
			a.log.Info("Ignoring claim due to address already in status")
		}
	}

	return reconcile.Result{}, nil
}

func (a *IPPoolClaimProcessor) InjectClient(c client.Client) error {
	a.Client = c
	a.log.Info("Set client for claim processor")
	return nil
}

func (a *IPPoolController) LoadPool(ctx context.Context, pool *ipamcontrollerv1.IPPool) error {
	a.log.V(4).Info(fmt.Sprintf("Loading pool: %v", pool.Name))

	// Initialize pool
	err := mgmt.InitializePool(ctx, pool)
	if err == nil {
		// Let's get all IPAddresses and see what has been already claimed to sync
		// the pool
		options := client.ListOptions{
			Namespace: pool.Namespace,
		}
		ipList := ipamv1.IPAddressList{}
		err = a.List(ctx, &ipList, &options)
		for _, ip := range ipList.Items {
			if ip.Spec.PoolRef.Name == pool.Name {
				a.log.Info(fmt.Sprintf("Found IP: %v", ip.Spec.Address))
				err = mgmt.ClaimIPAddress(ctx, pool, ip)
				if err != nil {
					a.log.Error(err, fmt.Sprintf("An error occurred when trying to claim IP %v: %v", ip.Spec.Address, err))
				} else {
					a.log.Info(fmt.Sprintf("IP %v is not part of pool %v", ip.Spec.Address, pool.Name))
				}
			}
		}
	}
	return err
}

func (a *IPPoolController) RemovePool(ctx context.Context, pool string) error {
	a.log.Info(fmt.Sprintf("Removing pool %v", pool))
	ipAddresses := &ipamv1.IPAddressList{}
	err := a.Client.List(ctx, ipAddresses)
	if err != nil {
		a.log.Error(err, "Unable to get IPAddresses")
		return err
	}

	a.log.Info("Searching for linked IPAddresses...")
	for _, ip := range ipAddresses.Items {
		a.log.Info(fmt.Sprintf("Checking IPAddress: %v", ip.Name))
		if fmt.Sprintf("%v/%v", ip.Namespace, ip.Spec.PoolRef.Name) == pool {
			a.log.Info(fmt.Sprintf("Deleting ipaddress CR %v", ip.Name))
			err = mgmt.ReleaseIPConfiguration(ctx, &ip)
			if err != nil {
				a.log.Error(err, "Error occurred while releasing IP")
			}
			err = a.Delete(ctx, &ip)
			if err != nil {
				a.log.Error(err, "Error occurred while cleaning up IP")
			}
		}
	}

	a.log.Info("Removing pool from mgmt...")
	err = mgmt.RemovePool(ctx, pool)
	if err != nil {
		a.log.Error(err, "Error removing pool from mgmt")
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (a *IPPoolController) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&ipamcontrollerv1.IPPool{}).
		Complete(a); err != nil {
		return fmt.Errorf("could not set up controller for ip pool: %w", err)
	}

	// Set up API helpers from the manager.
	a.Client = mgr.GetClient()
	a.Scheme = mgr.GetScheme()
	a.Recorder = mgr.GetEventRecorderFor("ip-pool-controller")
	a.RESTMapper = mgr.GetRESTMapper()

	return nil
}

func (a *IPPoolController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	mu.Lock()
	defer mu.Unlock()

	a.log.Info(fmt.Sprintf("Reconciling request %v", req))
	defer a.log.Info(fmt.Sprintf("Finished reconciling request %v", req))

	pool := &ipamcontrollerv1.IPPool{}
	poolKey := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}

	if err := a.Get(ctx, poolKey, pool); err != nil {
		a.log.Error(err, "got error")
		switch t := err.(type) {
		default:
			a.log.Info(fmt.Sprintf("Type: %v", t))

		}
		if strings.Contains(fmt.Sprintf("%v", err), "not found") {
			a.log.Info("Handling remove of claim")
			_ = a.RemovePool(ctx, fmt.Sprintf("%v", req))
			return reconcile.Result{}, nil
		} else {
			return reconcile.Result{}, err
		}
	}
	a.log.Info(fmt.Sprintf("Got Pool %v", pool.Name))
	if err := a.LoadPool(ctx, pool); err != nil {
		a.log.Error(err, "Unable to load pool")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (a *IPPoolController) InjectClient(c client.Client) error {
	a.Client = c
	return nil
}
