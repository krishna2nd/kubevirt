package watch

import (
	"github.com/jeevatkm/go-model"
	kubeapi "k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/errors"
	metav1 "k8s.io/client-go/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/logging"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

type vmResourceEventHandler struct {
	VMService services.VMService `inject:""`
	restCli   *rest.RESTClient
}

func NewVMResourceEventHandler() (kubecli.ResourceEventHandler, error) {
	restClient, err := kubecli.GetRESTClient()
	if err != nil {
		return nil, err
	}
	return &vmResourceEventHandler{restCli: restClient}, nil
}

func NewVMInformer(handler kubecli.ResourceEventHandler) (*cache.Controller, error) {
	restClient, err := kubecli.GetRESTClient()
	if err != nil {
		return nil, err
	}
	vmCacheSource := cache.NewListWatchFromClient(restClient, "vms", kubeapi.NamespaceDefault, fields.Everything())
	_, ctl := kubecli.NewInformer(vmCacheSource, &v1.VM{}, 0, handler)
	return ctl, nil
}

func NewVMCache() (cache.SharedInformer, error) {
	restClient, err := kubecli.GetRESTClient()
	if err != nil {
		return nil, err
	}
	vmCacheSource := cache.NewListWatchFromClient(restClient, "vms", kubeapi.NamespaceDefault, fields.Everything())
	informer := cache.NewSharedInformer(vmCacheSource, &v1.VM{}, 0)
	return informer, nil
}

func processVM(v *vmResourceEventHandler, obj *v1.VM) error {
	defer kubecli.CatchPanic()
	//TODO: Field selectors are not yet working for TPRs
	if obj.Status.Phase == "" {
		vm := v1.VM{}
		// Deep copy the object, so that we can savely manipulate it
		model.Copy(&vm, obj)
		logger := logging.DefaultLogger().Object(&vm)
		// Create a pod for the specified VM
		//Three cases where this can fail:
		// 1) VM pods exist from old definition // 2) VM pods exist from previous start attempt and updating the VM definition failed
		//    below
		// 3) Technical difficulties, we can't reach the apiserver
		// For case (1) this loop is not responsible. virt-handler or another loop is
		// responsible.
		// For case (2) we want to delete the VM first and then start over again.

		// TODO move defaulting to virt-api
		if vm.Spec.Domain == nil {
			spec := v1.NewMinimalDomainSpec(vm.GetObjectMeta().GetName())
			vm.Spec.Domain = spec
		}
		vm.Spec.Domain.UUID = string(vm.GetObjectMeta().GetUID())
		vm.Spec.Domain.Devices.Emulator = "/usr/local/bin/qemu-x86_64"
		vm.Spec.Domain.Name = vm.GetObjectMeta().GetName()

		// TODO get rid of these service calls
		if err := v.VMService.StartVM(&vm); err != nil {
			logger.Error().Msg("Defining a target pod for the VM.")
			pl, err := v.VMService.GetRunningPods(&vm)
			if err != nil {
				// TODO detect if communication error and backoff
				logger.Error().Reason(err).Msg("Getting all running Pods for the VM failed.")
				return cache.ErrRequeue{Err: err}
			}
			for _, p := range pl.Items {
				if p.GetObjectMeta().GetLabels()["kubevirt.io/vmUID"] == string(vm.GetObjectMeta().GetUID()) {
					// Pod from incomplete initialization detected, cleaning up
					logger.Error().Msgf("Found orphan pod with name '%s' for VM.", p.GetName())
					err = v.VMService.DeleteVM(&vm)
					if err != nil {
						// TODO detect if communication error and do backoff
						logger.Critical().Reason(err).Msgf("Deleting orphaned pod with name '%s' for VM failed.", p.GetName())
						return cache.ErrRequeue{Err: err}
					}
				} else {
					// TODO virt-api should make sure this does not happen. For now don't ask and clean up.
					// Pod from old VM object detected,
					logger.Error().Msgf("Found orphan pod with name '%s' for deleted VM.", p.GetName())
					err = v.VMService.DeleteVM(&vm)
					if err != nil {
						// TODO detect if communication error and backoff
						logger.Critical().Reason(err).Msgf("Deleting orphaned pod with name '%s' for VM failed.", p.GetName())
						return cache.ErrRequeue{Err: err}
					}
				}
			}
			return cache.ErrRequeue{Err: err}
		}
		// Mark the VM as "initialized". After the created Pod above is scheduled by
		// kubernetes, virt-handler can take over.
		//Three cases where this can fail:
		// 1) VM spec got deleted
		// 2) VM  spec got updated by the user
		// 3) Technical difficulties, we can't reach the apiserver
		// For (1) we don't want to retry, the pods will time out and fail. For (2) another
		// object got enqueued already. It will fail above until the created pods time out.
		// For (3) we want to enqueue again. If we don't do that the created pods will time out and we will
		// not get any updates
		vm.Status.Phase = v1.Scheduling
		if err := v.restCli.Put().Resource("vms").Body(&vm).Name(vm.ObjectMeta.Name).Namespace(kubeapi.NamespaceDefault).Do().Error(); err != nil {
			logger.Error().Reason(err).Msg("Updating the VM state to 'Scheduling' failed.")
			if e, ok := err.(*errors.StatusError); ok {
				if e.Status().Reason == metav1.StatusReasonNotFound ||
					e.Status().Reason == metav1.StatusReasonConflict {
					// Nothing to do for us, VM got either deleted in the meantime or a newer version is enqueued already
					return nil
				}
			}
			// TODO backoff policy here
			return cache.ErrRequeue{Err: err}
		}
		logger.Info().Msg("Handing over the VM to the scheduler succeeded.")
	}
	return nil
}

func (v *vmResourceEventHandler) OnAdd(obj interface{}) error {
	return processVM(v, obj.(*v1.VM))
}

func (v *vmResourceEventHandler) OnUpdate(oldObj, newObj interface{}) error {
	return processVM(v, newObj.(*v1.VM))
}

func (v *vmResourceEventHandler) OnDelete(obj interface{}) error {
	vm := obj.(*v1.VM)
	logger := logging.DefaultLogger().Object(vm)
	// TODO make sure the grace period is big enough that virt-handler can stop the VM the libvirt way
	// TODO maybe add a SIGTERM delay to virt-launcher in combination with a grace periode on the delete?
	err := v.VMService.DeleteVM(obj.(*v1.VM))
	if err != nil {
		logger.Error().Reason(err).Msg("Deleting VM target Pod failed.")
		return cache.ErrRequeue{Err: err}
	}
	logger.Info().Msg("Deleting VM target Pod succeeded.")
	return nil
}
