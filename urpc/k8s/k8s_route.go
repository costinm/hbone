package k8s

// TCPRoute is the Schema for the TCPRoute resource.
type TCPRoute struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata,omitempty"`

	Spec   TCPRouteSpec   `json:"spec,omitempty"`
	Status TCPRouteStatus `json:"status,omitempty"`
}

// TCPRouteSpec defines the desired state of TCPRoute
type TCPRouteSpec struct {
	// Rules are a list of TCP matchers and actions.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Rules []TCPRouteRule `json:"rules"`

	// Gateways defines which Gateways can use this Route.
	//
	// +kubebuilder:default={allow: "SameNamespace"}
	Gateways RouteGateways `json:"gateways,omitempty"`
}

// GatewayAllowType specifies which Gateways should be allowed to use a Route.
type GatewayAllowType string

const (
	// GatewayAllowAll indicates that all Gateways will be able to use this
	// route.
	GatewayAllowAll GatewayAllowType = "All"
	// GatewayAllowFromList indicates that only Gateways that have been
	// specified in GatewayRefs will be able to use this route.
	GatewayAllowFromList GatewayAllowType = "FromList"
	// GatewayAllowSameNamespace indicates that only Gateways within the same
	// namespace will be able to use this route.
	GatewayAllowSameNamespace GatewayAllowType = "SameNamespace"
)

// RouteGateways defines which Gateways will be able to use a route. If this
// field results in preventing the selection of a Route by a Gateway, an
// "Admitted" condition with a status of false must be set for the Gateway on
// that Route.
type RouteGateways struct {
	// Allow indicates which Gateways will be allowed to use this route.
	// Possible values are:
	// * All: Gateways in any namespace can use this route.
	// * FromList: Only Gateways specified in GatewayRefs may use this route.
	// * SameNamespace: Only Gateways in the same namespace may use this route.
	// +kubebuilder:validation:Enum=All;FromList;SameNamespace
	// +kubebuilder:default=SameNamespace
	Allow GatewayAllowType `json:"allow,omitempty"`
	// GatewayRefs must be specified when Allow is set to "FromList". In that
	// case, only Gateways referenced in this list will be allowed to use this
	// route. This field is ignored for other values of "Allow".
	// +optional
	GatewayRefs []GatewayReference `json:"gatewayRefs,omitempty"`
}

// RouteStatus defines the observed state that is required across
// all route types.
type RouteStatus struct {
	// Gateways is a list of the Gateways that are associated with the
	// route, and the status of the route with respect to each of these
	// Gateways. When a Gateway selects this route, the controller that
	// manages the Gateway should add an entry to this list when the
	// controller first sees the route and should update the entry as
	// appropriate when the route is modified.
	//
	// A maximum of 100 Gateways will be represented in this list. If this list
	// is full, there may be additional Gateways using this Route that are not
	// included in the list.
	//
	// +kubebuilder:validation:MaxItems=100
	Gateways []RouteGatewayStatus `json:"gateways"`
}

// RouteGatewayStatus describes the status of a route with respect to an
// associated Gateway.
type RouteGatewayStatus struct {
	// GatewayRef is a reference to a Gateway object that is associated with
	// the route.
	GatewayRef GatewayReference `json:"gatewayRef"`
	// Conditions describes the status of the route with respect to the
	// Gateway.  For example, the "Admitted" condition indicates whether the
	// route has been admitted or rejected by the Gateway, and why.  Note
	// that the route's availability is also subject to the Gateway's own
	// status conditions and listener status.
	//
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []Condition `json:"conditions,omitempty"`
}

// TCPRouteStatus defines the observed state of TCPRoute
type TCPRouteStatus struct {
	RouteStatus `json:",inline"`
}

// TCPRouteRule is the configuration for a given rule.
type TCPRouteRule struct {
	// Matches define conditions used for matching the rule against
	// incoming TCP connections. Each match is independent, i.e. this
	// rule will be matched if **any** one of the matches is satisfied.
	// If unspecified, all requests from the associated gateway TCP
	// listener will match.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Matches []TCPRouteMatch `json:"matches,omitempty"`

	// ForwardTo defines the backend(s) where matching requests should
	// be sent.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	ForwardTo []RouteForwardTo `json:"forwardTo"`
}

// TCPRouteMatch defines the predicate used to match connections to a
// given action.
type TCPRouteMatch struct {
	// ExtensionRef is an optional, implementation-specific extension to the
	// "match" behavior.  For example, resource "mytcproutematcher" in group
	// "networking.acme.io". If the referent cannot be found, the rule is not
	// included in the route. The controller should raise the "ResolvedRefs"
	// condition on the Gateway with the "DegradedRoutes" reason. The gateway
	// status for this route should be updated with a condition that describes
	// the error more specifically.
	//
	// Support: custom
	//
	// +optional
	ExtensionRef *LocalObjectReference `json:"extensionRef,omitempty"`
}

// +kubebuilder:object:root=true

// TCPRouteList contains a list of TCPRoute
type TCPRouteList struct {
	TypeMeta `json:",inline"`
	ListMeta `json:"metadata,omitempty"`
	Items    []TCPRoute `json:"items"`
}

// RouteForwardTo defines how a Route should forward a request.
type RouteForwardTo struct {
	// ServiceName refers to the name of the Service to forward matched requests
	// to. When specified, this takes the place of BackendRef. If both
	// BackendRef and ServiceName are specified, ServiceName will be given
	// precedence.
	//
	// If the referent cannot be found, the rule is not included in the route.
	// The controller should raise the "ResolvedRefs" condition on the Gateway
	// with the "DegradedRoutes" reason. The gateway status for this route should
	// be updated with a condition that describes the error more specifically.
	//
	// The protocol to use is defined using AppProtocol field (introduced in
	// Kubernetes 1.18) in the Service resource. In the absence of the
	// AppProtocol field a `networking.x-k8s.io/app-protocol` annotation on the
	// BackendPolicy resource may be used to define the protocol. If the
	// AppProtocol field is available, this annotation should not be used. The
	// AppProtocol field, when populated, takes precedence over the annotation
	// in the BackendPolicy resource. For custom backends, it is encouraged to
	// add a semantically-equivalent field in the Custom Resource Definition.
	//
	// Support: Core
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ServiceName *string `json:"serviceName,omitempty"`

	// BackendRef is a reference to a backend to forward matched requests to. If
	// both BackendRef and ServiceName are specified, ServiceName will be given
	// precedence.
	//
	// If the referent cannot be found, the rule is not included in the route.
	// The controller should raise the "ResolvedRefs" condition on the Gateway
	// with the "DegradedRoutes" reason. The gateway status for this route should
	// be updated with a condition that describes the error more specifically.
	//
	//
	// Support: Custom
	//
	// +optional
	BackendRef *LocalObjectReference `json:"backendRef,omitempty"`

	// Port specifies the destination port number to use for the
	// backend referenced by the ServiceName or BackendRef field.
	//
	// Support: Core
	Port PortNumber `json:"port"`

	// Weight specifies the proportion of HTTP requests forwarded to the backend
	// referenced by the ServiceName or BackendRef field. This is computed as
	// weight/(sum of all weights in this ForwardTo list). For non-zero values,
	// there may be some epsilon from the exact proportion defined here
	// depending on the precision an implementation supports. Weight is not a
	// percentage and the sum of weights does not need to equal 100.
	//
	// If only one backend is specified and it has a weight greater than 0, 100%
	// of the traffic is forwarded to that backend. If weight is set to 0, no
	// traffic should be forwarded for this entry. If unspecified, weight
	// defaults to 1.
	//
	// Support: Extended
	//
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	Weight int32 `json:"weight,omitempty"`
}
