/*
Copyright 2020 The Knative Authors

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

package broker

import (
	"context"
	"fmt"

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

	naming "knative.dev/eventing-rabbitmq/pkg/rabbitmqnaming"
	"knative.dev/eventing-rabbitmq/pkg/reconciler/broker/resources"
	triggerresources "knative.dev/eventing-rabbitmq/pkg/reconciler/trigger/resources"
	"knative.dev/eventing-rabbitmq/third_party/pkg/apis/rabbitmq.com/v1beta1"
	rabbitclientset "knative.dev/eventing-rabbitmq/third_party/pkg/client/clientset/versioned"
	rabbitlisters "knative.dev/eventing-rabbitmq/third_party/pkg/client/listers/rabbitmq.com/v1beta1"
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
	rabbitClientSet   rabbitclientset.Interface

	// listers index properties about resources
	brokerLister     eventinglisters.BrokerLister
	serviceLister    corev1listers.ServiceLister
	endpointsLister  corev1listers.EndpointsLister
	secretLister     corev1listers.SecretLister
	deploymentLister appsv1listers.DeploymentLister
	rabbitLister     apisduck.InformerFactory
	exchangeLister   rabbitlisters.ExchangeLister
	queueLister      rabbitlisters.QueueLister
	bindingLister    rabbitlisters.BindingLister

	ingressImage              string
	ingressServiceAccountName string

	// Dynamic tracker to track KResources. In particular, it tracks the dependency between Triggers and Sources.
	kresourceTracker duck.ListableTracker

	// Dynamic tracker to track AddressableTypes. In particular, it tracks DLX sinks.
	addressableTracker duck.ListableTracker
	uriResolver        *resolver.URIResolver

	// If specified, only reconcile brokers with these labels
	brokerClass string

	// Image to use for the DeadLetterSink dispatcher
	dispatcherImage string
}

// Check that our Reconciler implements Interface
var _ brokerreconciler.Interface = (*Reconciler)(nil)

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

	if !isUsingOperator(b) {
		MarkExchangeFailed(&b.Status, "ReconcileFailure", "using secret is not supported with this controller")
		return nil
	}
	args.RabbitMQCluster = b.Spec.Config.Name
	return r.reconcileUsingCRD(ctx, b, args)
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

func (r *Reconciler) reconcileExchange(ctx context.Context, args *resources.ExchangeArgs) (*v1beta1.Exchange, error) {
	want := resources.NewExchange(ctx, args)
	current, err := r.exchangeLister.Exchanges(args.Broker.Namespace).Get(naming.BrokerExchangeName(args.Broker, args.DLX))
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Debugw("Creating rabbitmq exchange", zap.String("exchange name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Exchanges(args.Broker.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Exchanges(args.Broker.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileQueue(ctx context.Context, b *eventingv1.Broker) (*v1beta1.Queue, error) {
	queueName := naming.CreateBrokerDeadLetterQueueName(b)
	want := triggerresources.NewQueue(ctx, b, nil)
	current, err := r.queueLister.Queues(b.Namespace).Get(queueName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Debugw("Creating rabbitmq exchange", zap.String("queue name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileBinding(ctx context.Context, b *eventingv1.Broker) (*v1beta1.Binding, error) {
	// We can use the same name for queue / binding to keep things simpler
	bindingName := naming.CreateBrokerDeadLetterQueueName(b)

	want, err := triggerresources.NewBinding(ctx, b, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create the binding spec: %w", err)
	}
	current, err := r.bindingLister.Bindings(b.Namespace).Get(bindingName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Infow("Creating rabbitmq binding", zap.String("binding name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Bindings(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Bindings(b.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileUsingCRD(ctx context.Context, b *eventingv1.Broker, args *resources.ExchangeArgs) error {
	logging.FromContext(ctx).Info("Reconciling exchange")
	exchange, err := r.reconcileExchange(ctx, args)
	if err != nil {
		MarkExchangeFailed(&b.Status, "ExchangeFailure", fmt.Sprintf("Failed to reconcile exchange %q: %s", naming.BrokerExchangeName(args.Broker, false), err))
		return err
	}
	if exchange != nil {
		if !isReady(exchange.Status.Conditions) {
			logging.FromContext(ctx).Warnf("Exchange %q is not ready", exchange.Name)
			MarkExchangeFailed(&b.Status, "ExchangeFailure", fmt.Sprintf("exchange %q is not ready", exchange.Name))
			return nil
		}
	}
	args.DLX = true

	logging.FromContext(ctx).Info("Reconciling DLX exchange")
	dlxExchange, err := r.reconcileExchange(ctx, args)
	if err != nil {
		MarkExchangeFailed(&b.Status, "ExchangeFailure", fmt.Sprintf("Failed to reconcile DLX exchange %q: %s", naming.BrokerExchangeName(args.Broker, true), err))
		return err
	}
	if dlxExchange != nil {
		if !isReady(dlxExchange.Status.Conditions) {
			logging.FromContext(ctx).Warnf("DLX exchange %q is not ready", dlxExchange.Name)
			MarkExchangeFailed(&b.Status, "ExchangeFailure", fmt.Sprintf("DLX exchange %q is not ready", dlxExchange.Name))
			return nil
		}
	}
	MarkExchangeReady(&b.Status)

	logging.FromContext(ctx).Info("Reconciling queue")
	queue, err := r.reconcileQueue(ctx, b)
	if err != nil {
		MarkDLXFailed(&b.Status, "QueueFailure", fmt.Sprintf("Failed to reconcile Dead Letter Queue %q : %s", naming.CreateBrokerDeadLetterQueueName(b), err))
		return err
	}
	if queue != nil {
		if !isReady(queue.Status.Conditions) {
			logging.FromContext(ctx).Warnf("Queue %q is not ready", queue.Name)
			MarkDLXFailed(&b.Status, "QueueFailure", fmt.Sprintf("Dead Letter Queue %q is not ready", queue.Name))
			return nil
		}
	}

	MarkDLXReady(&b.Status)

	logging.FromContext(ctx).Info("Reconciling binding")
	binding, err := r.reconcileBinding(ctx, b)
	if err != nil {
		// NB, binding has the same name as the queue.
		MarkDeadLetterSinkFailed(&b.Status, "DLQ binding", fmt.Sprintf("Failed to reconcile DLQ binding %q : %s", naming.CreateBrokerDeadLetterQueueName(b), err))
		return err
	}
	if binding != nil {
		if !isReady(binding.Status.Conditions) {
			logging.FromContext(ctx).Warnf("Binding %q is not ready", binding.Name)
			MarkDeadLetterSinkFailed(&b.Status, "DLQ binding", fmt.Sprintf("DLQ binding %q is not ready", binding.Name))
			return nil
		}
	}
	MarkDeadLetterSinkReady(&b.Status)

	return r.reconcileCommonIngressResources(ctx, resources.MakeSecret(args), b)
}

func isReady(conditions []v1beta1.Condition) bool {
	numConditions := len(conditions)
	// If there are no conditions at all, the resource probably hasn't been reconciled yet => not ready
	if numConditions == 0 {
		return false
	}
	for _, c := range conditions {
		if c.Status == corev1.ConditionTrue {
			numConditions--
		}
	}
	return numConditions == 0
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
