/*
Copyright 2021 The Knative Authors

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

package brokerstandalone

import (
	"context"
	"fmt"
	"net/http"

	"go.uber.org/zap"
	v1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/network"

	dialer "knative.dev/eventing-rabbitmq/pkg/amqp"
	naming "knative.dev/eventing-rabbitmq/pkg/rabbitmqnaming"
	"knative.dev/eventing-rabbitmq/pkg/reconciler/brokerstandalone/resources"
	triggerresources "knative.dev/eventing-rabbitmq/pkg/reconciler/triggerstandalone/resources"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	clientset "knative.dev/eventing/pkg/client/clientset/versioned"
	brokerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1/broker"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1"

	apisduck "knative.dev/pkg/apis/duck"

	"knative.dev/eventing/pkg/duck"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
)

type Reconciler struct {
	eventingClientSet clientset.Interface
	dynamicClientSet  dynamic.Interface
	kubeClientSet     kubernetes.Interface

	// listers index properties about resources
	brokerLister     eventinglisters.BrokerLister
	serviceLister    corev1listers.ServiceLister
	endpointsLister  corev1listers.EndpointsLister
	secretLister     corev1listers.SecretLister
	deploymentLister appsv1listers.DeploymentLister
	rabbitLister     apisduck.InformerFactory

	ingressImage              string
	ingressServiceAccountName string

	// Dynamic tracker to track KResources. In particular, it tracks the dependency between Triggers and Sources.
	kresourceTracker duck.ListableTracker

	// Dynamic tracker to track AddressableTypes. In particular, it tracks DLX sinks.
	addressableTracker duck.ListableTracker
	uriResolver        *resolver.URIResolver

	// If specified, only reconcile brokers with these labels
	brokerClass string

	// Which dialer to use.
	dialerFunc dialer.DialerFunc

	// Image to use for the DeadLetterSink dispatcher
	dispatcherImage string

	// Which HTTP transport to use
	transport http.RoundTripper
	// For testing...
	adminURL string
}

// Check that our Reconciler implements Interface
var _ brokerreconciler.Interface = (*Reconciler)(nil)
var _ brokerreconciler.Finalizer = (*Reconciler)(nil)

// ReconcilerArgs are the arguments needed to create a broker.Reconciler.
type ReconcilerArgs struct {
	IngressImage              string
	IngressServiceAccountName string
}

const (
	BrokerConditionExchange       apis.ConditionType = "ExchangeReady"
	BrokerConditionDLX            apis.ConditionType = "DLXReady"
	BrokerConditionDeadLetterSink apis.ConditionType = "DeadLetterSinkReady"
	BrokerConditionSecret         apis.ConditionType = "SecretReady"
	BrokerConditionIngress        apis.ConditionType = "IngressReady"
	BrokerConditionAddressable    apis.ConditionType = "Addressable"
)

var rabbitBrokerCondSet = apis.NewLivingConditionSet(
	BrokerConditionExchange,
	BrokerConditionDLX,
	BrokerConditionDeadLetterSink,
	BrokerConditionSecret,
	BrokerConditionIngress,
	BrokerConditionAddressable,
)

// isUsingOperator checks the Spec for a Broker and determines if we should be using the
// messaging-topology-operator or the libraries.
func isUsingOperator(b *eventingv1.Broker) bool {
	if b != nil && b.Spec.Config != nil {
		return b.Spec.Config.Kind == "RabbitmqCluster"
	}
	return false
}

func (r *Reconciler) ReconcileKind(ctx context.Context, b *eventingv1.Broker) pkgreconciler.Event {
	logging.FromContext(ctx).Infow("Reconciling", zap.Any("Broker", b))

	// 1. RabbitMQ Exchange
	// 2. Ingress Deployment
	// 3. K8s Service that points to the Ingress Deployment
	args, err := r.getExchangeArgs(ctx, b)
	if err != nil {
		MarkExchangeFailed(&b.Status, "ExchangeCredentialsUnavailable", "Failed to get arguments for creating exchange: %s", err)
		return err
	}

	if isUsingOperator(b) {
		// TODO: Mark as error since we can't reconcile these.
		return fmt.Errorf("WON'T GO")
	} else {
		return r.reconcileUsingLibraries(ctx, b, args)
	}
}

func (r *Reconciler) FinalizeKind(ctx context.Context, b *eventingv1.Broker) pkgreconciler.Event {
	if isUsingOperator(b) {
		// Everything gets cleaned up by garbage collection in this case.
		return nil
	}
	args, err := r.getExchangeArgs(ctx, b)
	if err != nil {
		// TODO: Problem here is that depending on the kind of error we get back, say there's no
		// RabbitMQ cluster anymore would mean that we couldn't necessarily delete underlying Rabbit
		// resources. But leaving the Broker around seems worse because the user would have to manually
		// remove the finalizer. So, log it and allow the removal of the resource.
		logging.FromContext(ctx).Errorw("Failed to get Exchange args, there might be leaked rabbit resources", zap.Error(err))
		return nil
	}
	if err := resources.DeleteExchange(args); err != nil {
		logging.FromContext(ctx).Errorw("Problem deleting exchange", zap.Error(err))
	}
	args.DLX = true
	err = resources.DeleteExchange(args)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem deleting DLX exchange", zap.Error(err))
	}
	queueArgs := &triggerresources.QueueArgs{
		QueueName:   naming.CreateBrokerDeadLetterQueueName(b),
		RabbitmqURL: args.RabbitMQURL.String(),
	}
	err = triggerresources.DeleteQueue(r.dialerFunc, queueArgs)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem deleting DLX queue", zap.Error(err))
	}
	return nil
}

// reconcileSecret reconciles the K8s Secret 's'.
func (r *Reconciler) reconcileSecret(ctx context.Context, s *corev1.Secret) error {
	current, err := r.secretLister.Secrets(s.Namespace).Get(s.Name)
	if apierrs.IsNotFound(err) {
		_, err = r.kubeClientSet.CoreV1().Secrets(s.Namespace).Create(ctx, s, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepDerivative(s.StringData, current.StringData) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.StringData = s.StringData
		_, err = r.kubeClientSet.CoreV1().Secrets(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// reconcileDeployment reconciles the K8s Deployment 'd'.
func (r *Reconciler) reconcileDeployment(ctx context.Context, d *v1.Deployment) error {
	current, err := r.deploymentLister.Deployments(d.Namespace).Get(d.Name)
	if apierrs.IsNotFound(err) {
		_, err = r.kubeClientSet.AppsV1().Deployments(d.Namespace).Create(ctx, d, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !equality.Semantic.DeepDerivative(d.Spec.Template, current.Spec.Template) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = d.Spec
		_, err = r.kubeClientSet.AppsV1().Deployments(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// reconcileService reconciles the K8s Service 'svc'.
func (r *Reconciler) reconcileService(ctx context.Context, svc *corev1.Service) (*corev1.Endpoints, error) {
	current, err := r.serviceLister.Services(svc.Namespace).Get(svc.Name)
	if apierrs.IsNotFound(err) {
		current, err = r.kubeClientSet.CoreV1().Services(svc.Namespace).Create(ctx, svc, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// spec.clusterIP is immutable and is set on existing services. If we don't set this to the same value, we will
	// encounter an error while updating.
	svc.Spec.ClusterIP = current.Spec.ClusterIP
	if !equality.Semantic.DeepDerivative(svc.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = svc.Spec
		if _, err = r.kubeClientSet.CoreV1().Services(current.Namespace).Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}

	return r.endpointsLister.Endpoints(svc.Namespace).Get(svc.Name)
}

// reconcileIngressDeploymentCRD reconciles the Ingress Deployment.
func (r *Reconciler) reconcileIngressDeployment(ctx context.Context, b *eventingv1.Broker) error {
	expected := resources.MakeIngressDeployment(&resources.IngressArgs{
		Broker:             b,
		Image:              r.ingressImage,
		RabbitMQSecretName: resources.SecretName(b.Name),
		BrokerUrlSecretKey: resources.BrokerURLSecretKey,
	})
	return r.reconcileDeployment(ctx, expected)
}

// reconcileIngressService reconciles the Ingress Service.
func (r *Reconciler) reconcileIngressService(ctx context.Context, b *eventingv1.Broker) (*corev1.Endpoints, error) {
	expected := resources.MakeIngressService(b)
	return r.reconcileService(ctx, expected)
}

// reconcileDLXDispatcherDeployment reconciles Brokers DLX dispatcher deployment.
func (r *Reconciler) reconcileDLXDispatcherDeployment(ctx context.Context, b *eventingv1.Broker, sub *apis.URL) error {
	// If there's a sub, then reconcile the deployment as usual.
	if sub != nil {
		expected := resources.MakeDispatcherDeployment(&resources.DispatcherArgs{
			Broker: b,
			Image:  r.dispatcherImage,
			//ServiceAccountName string
			RabbitMQSecretName: resources.SecretName(b.Name),
			QueueName:          naming.CreateBrokerDeadLetterQueueName(b),
			BrokerUrlSecretKey: resources.BrokerURLSecretKey,
			Subscriber:         sub,
			BrokerIngressURL:   b.Status.Address.URL,
		})
		return r.reconcileDeployment(ctx, expected)
	}
	// However if there's not, then ensure that one doesn't exist and delete it if does.
	dispatcherName := resources.DispatcherName(b.Name)
	_, err := r.deploymentLister.Deployments(b.Namespace).Get(dispatcherName)
	if apierrs.IsNotFound(err) {
		// Not there, we're good.
		return nil
	} else if err != nil {
		return err
	}
	// It's there but it shouldn't, so delete.
	return r.kubeClientSet.AppsV1().Deployments(b.Namespace).Delete(ctx, dispatcherName, metav1.DeleteOptions{})
}

func (r *Reconciler) reconcileDLQBinding(ctx context.Context, b *eventingv1.Broker) error {
	args, err := r.getExchangeArgs(ctx, b)
	if err != nil {
		return err
	}

	err = triggerresources.MakeDLQBinding(r.transport, &triggerresources.BindingArgs{
		Broker:     b,
		RoutingKey: "",
		BrokerURL:  args.RabbitMQURL.String(),
		AdminURL:   r.adminURL,
		QueueName:  naming.CreateBrokerDeadLetterQueueName(b),
	})
	if err != nil {
		logging.FromContext(ctx).Error("Problem declaring Broker DLQ Binding", zap.Error(err))
		return err
	}
	return nil
}

func (r *Reconciler) reconcileUsingLibraries(ctx context.Context, b *eventingv1.Broker, args *resources.ExchangeArgs) error {
	// 1. RabbitMQ Exchange
	// 2. Ingress Deployment
	// 3. K8s Service that points to the Ingress Deployment
	s, err := resources.DeclareExchange(r.dialerFunc, args)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem creating RabbitMQ Exchange", zap.Error(err))
		MarkExchangeFailed(&b.Status, "ExchangeFailure", "Failed to create exchange: %s", err)
		return err
	}
	MarkExchangeReady(&b.Status)
	// Always just create the DLX, if we want to only create if there's a delivery spec to save
	// resources, revisit as in if it hogs up resources too much even if it's not being used for
	// example.
	args.DLX = true
	_, err = resources.DeclareExchange(r.dialerFunc, args)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem creating RabbitMQ DLX", zap.Error(err))
		MarkDLXFailed(&b.Status, "ExchangeFailure", "Failed to create DLX: %s", err)
		return err
	}

	queueArgs := &triggerresources.QueueArgs{
		QueueName:   naming.CreateBrokerDeadLetterQueueName(b),
		RabbitmqURL: args.RabbitMQURL.String(),
	}
	_, err = triggerresources.DeclareQueue(r.dialerFunc, queueArgs)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem creating RabbitMQ Dead Letter Queue", zap.Error(err))
		MarkDLXFailed(&b.Status, "QueueFailure", "Failed to create Dead Letter Queue: %s", err)
		return err
	}
	MarkDLXReady(&b.Status)

	if err := r.reconcileDLQBinding(ctx, b); err != nil {
		logging.FromContext(ctx).Error("Problem reconciling DLX Binding", zap.Error(err))
		MarkDeadLetterSinkFailed(&b.Status, "DLQ binding", "%v", err)
		return err
	}
	MarkDeadLetterSinkReady(&b.Status)

	return r.reconcileCommonIngressResources(ctx, s, b)
}

// reconcileCommonIngressResources that are shared between implementations using CRDs or libraries.
func (r *Reconciler) reconcileCommonIngressResources(ctx context.Context, s *corev1.Secret, b *eventingv1.Broker) error {
	if err := r.reconcileSecret(ctx, s); err != nil {
		logging.FromContext(ctx).Errorw("Problem reconciling Secret", zap.Error(err))
		MarkSecretFailed(&b.Status, "SecretFailure", "Failed to reconcile secret: %s", err)
		return err
	}
	MarkSecretReady(&b.Status)

	if err := r.reconcileIngressDeployment(ctx, b); err != nil {
		logging.FromContext(ctx).Errorw("Problem reconciling ingress Deployment", zap.Error(err))
		MarkIngressFailed(&b.Status, "DeploymentFailure", "Failed to reconcile deployment: %s", err)
		return err
	}

	ingressEndpoints, err := r.reconcileIngressService(ctx, b)
	if err != nil {
		logging.FromContext(ctx).Errorw("Problem reconciling ingress Service", zap.Error(err))
		MarkIngressFailed(&b.Status, "ServiceFailure", "Failed to reconcile service: %s", err)
		return err
	}
	PropagateIngressAvailability(&b.Status, ingressEndpoints)

	SetAddress(&b.Status, &apis.URL{
		Scheme: "http",
		Host:   network.GetServiceHostname(ingressEndpoints.GetName(), ingressEndpoints.GetNamespace()),
	})

	// If there's a Dead Letter Sink, then create a dispatcher for it. Note that this is for
	// the whole broker, unlike for the Trigger, where we create one dispatcher per Trigger.
	var dlsURI *apis.URL
	if b.Spec.Delivery != nil && b.Spec.Delivery.DeadLetterSink != nil {
		dlsURI, err = r.uriResolver.URIFromDestinationV1(ctx, *b.Spec.Delivery.DeadLetterSink, b)
		if err != nil {
			logging.FromContext(ctx).Error("Unable to get the DeadLetterSink URI", zap.Error(err))
			MarkDeadLetterSinkFailed(&b.Status, "Unable to get the DeadLetterSink's URI", "%v", err)
			return err
		}

		// TODO(vaikas): Set the custom annotation for resolved URI?...
		// TODO(vaikas): Should this be a first level BrokerStatus field?
	}

	// Note that if we didn't actually resolve the URI above, as in it's left as nil it's ok to pass here
	// it deals with it properly.
	if err := r.reconcileDLXDispatcherDeployment(ctx, b, dlsURI); err != nil {
		logging.FromContext(ctx).Error("Problem reconciling DLX dispatcher Deployment", zap.Error(err))
		MarkDeadLetterSinkFailed(&b.Status, "DeploymentFailure", "%v", err)
		return err
	}
	return nil
}
