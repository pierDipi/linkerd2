package cmd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/golang/protobuf/ptypes/duration"
	"github.com/linkerd/linkerd2/controller/api/util"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/pkg/addr"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/protohttp"
	"github.com/linkerd/linkerd2/pkg/tap"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
)

type renderTapEventFunc func(*pb.TapEvent, string) string

type tapOptions struct {
	namespace   string
	toResource  string
	toNamespace string
	maxRps      float32
	scheme      string
	method      string
	authority   string
	path        string
	output      string
}

type endpoint struct {
	IP       string            `json:"ip"`
	Port     uint32            `json:"port"`
	Metadata map[string]string `json:"metadata"`
}

type streamID struct {
	Base   uint32 `json:"base"`
	Stream uint64 `json:"stream"`
}

type requestInitEvent struct {
	ID        *streamID      `json:"id"`
	Method    *pb.HttpMethod `json:"method"`
	Scheme    *pb.Scheme     `json:"scheme"`
	Authority string         `json:"authority"`
	Path      string         `json:"path"`
}

type responseInitEvent struct {
	ID               *streamID          `json:"id"`
	SinceRequestInit *duration.Duration `json:"sinceRequestInit"`
	HTTPStatus       uint32             `json:"httpStatus"`
}

type responseEndEvent struct {
	ID                *streamID          `json:"id"`
	SinceRequestInit  *duration.Duration `json:"sinceRequestInit"`
	SinceResponseInit *duration.Duration `json:"sinceResponseInit"`
	ResponseBytes     uint64             `json:"responseBytes"`
	GrpcStatusCode    uint32             `json:"grpcStatusCode,omitempty"`
	ResetErrorCode    uint32             `json:"resetErrorCode,omitempty"`
}

type tapEvent struct {
	Source            *endpoint          `json:"source"`
	Destination       *endpoint          `json:"destination"`
	RouteMeta         map[string]string  `json:"routeMeta"`
	ProxyDirection    string             `json:"proxyDirection"`
	RequestInitEvent  *requestInitEvent  `json:"requestInitEvent,omitempty"`
	ResponseInitEvent *responseInitEvent `json:"responseInitEvent,omitempty"`
	ResponseEndEvent  *responseEndEvent  `json:"responseEndEvent,omitempty"`
}

func newTapOptions() *tapOptions {
	return &tapOptions{
		namespace:   "default",
		toResource:  "",
		toNamespace: "",
		maxRps:      100.0,
		scheme:      "",
		method:      "",
		authority:   "",
		path:        "",
		output:      "",
	}
}

func (o *tapOptions) validate() error {
	if o.output == "" || o.output == wideOutput || o.output == jsonOutput {
		return nil
	}

	return fmt.Errorf("output format \"%s\" not recognized", o.output)
}

func newCmdTap() *cobra.Command {
	options := newTapOptions()

	cmd := &cobra.Command{
		Use:   "tap [flags] (RESOURCE)",
		Short: "Listen to a traffic stream",
		Long: `Listen to a traffic stream.

  The RESOURCE argument specifies the target resource(s) to tap:
  (TYPE [NAME] | TYPE/NAME)

  Examples:
  * deploy
  * deploy/my-deploy
  * deploy my-deploy
  * ds/my-daemonset
  * job/my-job
  * ns/my-ns
  * sts
  * sts/my-statefulset

  Valid resource types include:
  * daemonsets
  * deployments
  * jobs
  * namespaces
  * pods
  * replicationcontrollers
  * statefulsets
  * services (only supported as a --to resource)`,
		Example: `  # tap the web deployment in the default namespace
  linkerd tap deploy/web

  # tap the web-dlbvj pod in the default namespace
  linkerd tap pod/web-dlbvj

  # tap the test namespace, filter by request to prod namespace
  linkerd tap ns/test --to ns/prod`,
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: util.ValidTargets,
		RunE: func(cmd *cobra.Command, args []string) error {
			requestParams := util.TapRequestParams{
				Resource:    strings.Join(args, "/"),
				Namespace:   options.namespace,
				ToResource:  options.toResource,
				ToNamespace: options.toNamespace,
				MaxRps:      options.maxRps,
				Scheme:      options.scheme,
				Method:      options.method,
				Authority:   options.authority,
				Path:        options.path,
			}

			err := options.validate()
			if err != nil {
				return fmt.Errorf("Validation error when executing tap command: %v", err)
			}

			req, err := util.BuildTapByResourceRequest(requestParams)
			if err != nil {
				return err
			}

			k8sAPI, err := k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, 0)
			if err != nil {
				return err
			}

			return requestTapByResourceFromAPI(os.Stdout, k8sAPI, req, options)
		},
	}

	cmd.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", options.namespace,
		"Namespace of the specified resource")
	cmd.PersistentFlags().StringVar(&options.toResource, "to", options.toResource,
		"Display requests to this resource")
	cmd.PersistentFlags().StringVar(&options.toNamespace, "to-namespace", options.toNamespace,
		"Sets the namespace used to lookup the \"--to\" resource; by default the current \"--namespace\" is used")
	cmd.PersistentFlags().Float32Var(&options.maxRps, "max-rps", options.maxRps,
		"Maximum requests per second to tap.")
	cmd.PersistentFlags().StringVar(&options.scheme, "scheme", options.scheme,
		"Display requests with this scheme")
	cmd.PersistentFlags().StringVar(&options.method, "method", options.method,
		"Display requests with this HTTP method")
	cmd.PersistentFlags().StringVar(&options.authority, "authority", options.authority,
		"Display requests with this :authority")
	cmd.PersistentFlags().StringVar(&options.path, "path", options.path,
		"Display requests with paths that start with this prefix")
	cmd.PersistentFlags().StringVarP(&options.output, "output", "o", options.output,
		fmt.Sprintf("Output format. One of: \"%s\", \"%s\"", wideOutput, jsonOutput))

	return cmd
}

func requestTapByResourceFromAPI(w io.Writer, k8sAPI *k8s.KubernetesAPI, req *pb.TapByResourceRequest, options *tapOptions) error {
	reader, body, err := tap.Reader(k8sAPI, req, 0)
	if err != nil {
		return err
	}
	defer body.Close()

	return writeTapEventsToBuffer(w, reader, req, options)
}

func writeTapEventsToBuffer(w io.Writer, tapByteStream *bufio.Reader, req *pb.TapByResourceRequest, options *tapOptions) error {
	writer := tabwriter.NewWriter(w, 0, 0, 0, ' ', tabwriter.AlignRight)

	var err error
	switch options.output {
	case "":
		err = renderTapEvents(tapByteStream, writer, renderTapEvent, "")
	case wideOutput:
		resource := req.GetTarget().GetResource().GetType()
		err = renderTapEvents(tapByteStream, writer, renderTapEvent, resource)
	case jsonOutput:
		err = renderTapEvents(tapByteStream, writer, renderTapEventJSON, "")
	}
	if err != nil {
		return err
	}
	writer.Flush()

	return nil
}

func renderTapEvents(tapByteStream *bufio.Reader, w *tabwriter.Writer, render renderTapEventFunc, resource string) error {
	for {
		log.Debug("Waiting for data...")
		event := pb.TapEvent{}
		err := protohttp.FromByteStreamToProtocolBuffers(tapByteStream, &event)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			break
		}
		_, err = fmt.Fprintln(w, render(&event, resource))
		if err != nil {
			return err
		}
	}

	return nil
}

// renderTapEvent renders a Public API TapEvent to a string.
func renderTapEvent(event *pb.TapEvent, resource string) string {
	dst := dst(event)
	src := src(event)

	proxy := "???"
	tls := ""
	switch event.GetProxyDirection() {
	case pb.TapEvent_INBOUND:
		proxy = "in " // A space is added so it aligns with `out`.
		tls = src.tlsStatus()
	case pb.TapEvent_OUTBOUND:
		proxy = "out"
		tls = dst.tlsStatus()
	default:
		// Too old for TLS.
	}

	flow := fmt.Sprintf("proxy=%s %s %s tls=%s",
		proxy,
		src.formatAddr(),
		dst.formatAddr(),
		tls,
	)

	// If `resource` is non-empty, then
	resources := ""
	if resource != "" {
		resources = fmt.Sprintf(
			"%s%s%s",
			src.formatResource(resource),
			dst.formatResource(resource),
			routeLabels(event),
		)
	}

	switch ev := event.GetHttp().GetEvent().(type) {
	case *pb.TapEvent_Http_RequestInit_:
		return fmt.Sprintf("req id=%d:%d %s :method=%s :authority=%s :path=%s%s",
			ev.RequestInit.GetId().GetBase(),
			ev.RequestInit.GetId().GetStream(),
			flow,
			ev.RequestInit.GetMethod().GetRegistered().String(),
			ev.RequestInit.GetAuthority(),
			ev.RequestInit.GetPath(),
			resources,
		)

	case *pb.TapEvent_Http_ResponseInit_:
		return fmt.Sprintf("rsp id=%d:%d %s :status=%d latency=%dµs%s",
			ev.ResponseInit.GetId().GetBase(),
			ev.ResponseInit.GetId().GetStream(),
			flow,
			ev.ResponseInit.GetHttpStatus(),
			ev.ResponseInit.GetSinceRequestInit().GetNanos()/1000,
			resources,
		)

	case *pb.TapEvent_Http_ResponseEnd_:
		switch eos := ev.ResponseEnd.GetEos().GetEnd().(type) {
		case *pb.Eos_GrpcStatusCode:
			return fmt.Sprintf(
				"end id=%d:%d %s grpc-status=%s duration=%dµs response-length=%dB%s",
				ev.ResponseEnd.GetId().GetBase(),
				ev.ResponseEnd.GetId().GetStream(),
				flow,
				codes.Code(eos.GrpcStatusCode),
				ev.ResponseEnd.GetSinceResponseInit().GetNanos()/1000,
				ev.ResponseEnd.GetResponseBytes(),
				resources,
			)

		case *pb.Eos_ResetErrorCode:
			return fmt.Sprintf(
				"end id=%d:%d %s reset-error=%+v duration=%dµs response-length=%dB%s",
				ev.ResponseEnd.GetId().GetBase(),
				ev.ResponseEnd.GetId().GetStream(),
				flow,
				eos.ResetErrorCode,
				ev.ResponseEnd.GetSinceResponseInit().GetNanos()/1000,
				ev.ResponseEnd.GetResponseBytes(),
				resources,
			)

		default:
			return fmt.Sprintf("end id=%d:%d %s duration=%dµs response-length=%dB%s",
				ev.ResponseEnd.GetId().GetBase(),
				ev.ResponseEnd.GetId().GetStream(),
				flow,
				ev.ResponseEnd.GetSinceResponseInit().GetNanos()/1000,
				ev.ResponseEnd.GetResponseBytes(),
				resources,
			)
		}

	default:
		return fmt.Sprintf("unknown %s", flow)
	}
}

// Map public API `TapEvent`s to `displayTapEvent`s
func mapPublicToDisplayTapEvent(event *pb.TapEvent) *tapEvent {
	// Map source endpoint
	sip := getIPAddress(event.GetSource().GetIp())
	sp := event.GetSource().GetPort()
	s := &endpoint{
		IP:       sip,
		Port:     sp,
		Metadata: event.GetSourceMeta().GetLabels(),
	}

	// Map destination endpoint
	dip := getIPAddress(event.GetDestination().GetIp())
	dp := event.GetDestination().GetPort()
	d := &endpoint{
		IP:       dip,
		Port:     dp,
		Metadata: event.GetDestinationMeta().GetLabels(),
	}

	// Map route metadata and proxy direction
	rm := event.GetRouteMeta().GetLabels()
	pdir := event.GetProxyDirection().String()

	return &tapEvent{
		Source:            s,
		Destination:       d,
		RouteMeta:         rm,
		ProxyDirection:    pdir,
		RequestInitEvent:  getRequestInitEvent(event.GetHttp()),
		ResponseInitEvent: getResponseInitEvent(event.GetHttp()),
		ResponseEndEvent:  getResponseEndEvent(event.GetHttp()),
	}
}

// renderTapEventJSON renders a Public API TapEvent to a string in JSON format.
func renderTapEventJSON(event *pb.TapEvent, _ string) string {
	m := mapPublicToDisplayTapEvent(event)
	e, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshalling JSON: %s\n", err)
	}
	return fmt.Sprintf("%s", e)
}

// src returns the source peer of a `TapEvent`.
func src(event *pb.TapEvent) peer {
	return peer{
		address:   event.GetSource(),
		labels:    event.GetSourceMeta().GetLabels(),
		direction: "src",
	}
}

// dst returns the destination peer of a `TapEvent`.
func dst(event *pb.TapEvent) peer {
	return peer{
		address:   event.GetDestination(),
		labels:    event.GetDestinationMeta().GetLabels(),
		direction: "dst",
	}
}

type peer struct {
	address   *pb.TcpAddress
	labels    map[string]string
	direction string
}

// formatAddr formats the peer's TCP address for the `src` or `dst` element in
// the tap output corresponding to this peer.
func (p *peer) formatAddr() string {
	return fmt.Sprintf(
		"%s=%s",
		p.direction,
		addr.PublicAddressToString(p.address),
	)
}

// formatResource returns a label describing what Kubernetes resources the peer
// belongs to. If the peer belongs to a resource of kind `resourceKind`, it will
// return a label for that resource; otherwise, it will fall back to the peer's
// pod name. Additionally, if the resource is not of type `namespace`, it will
// also add a label describing the peer's resource.
func (p *peer) formatResource(resourceKind string) string {
	var s string
	if resourceName, exists := p.labels[resourceKind]; exists {
		kind := resourceKind
		if short := k8s.ShortNameFromCanonicalResourceName(resourceKind); short != "" {
			kind = short
		}
		s = fmt.Sprintf(
			" %s_res=%s/%s",
			p.direction,
			kind,
			resourceName,
		)
	} else if pod, hasPod := p.labels[k8s.Pod]; hasPod {
		s = fmt.Sprintf(" %s_pod=%s", p.direction, pod)
	}
	if resourceKind != k8s.Namespace {
		if ns, hasNs := p.labels[k8s.Namespace]; hasNs {
			s += fmt.Sprintf(" %s_ns=%s", p.direction, ns)
		}
	}
	return s
}

func (p *peer) tlsStatus() string {
	return p.labels["tls"]
}

func routeLabels(event *pb.TapEvent) string {
	out := ""
	for key, val := range event.GetRouteMeta().GetLabels() {
		out = fmt.Sprintf("%s rt_%s=%s", out, key, val)
	}

	return out
}

func getIPAddress(ip *pb.IPAddress) string {
	var b []byte
	if ip.GetIpv6() != nil {
		b = make([]byte, 16)
		binary.BigEndian.PutUint64(b[:8], ip.GetIpv6().GetFirst())
		binary.BigEndian.PutUint64(b[8:], ip.GetIpv6().GetLast())
	} else if ip.GetIpv4() != 0 {
		b = make([]byte, 4)
		binary.BigEndian.PutUint32(b, ip.GetIpv4())
	}
	return net.IP(b).String()
}

// Attempt to map a `TapEvent_Http_RequestInit event to a `requestInitEvent`
func getRequestInitEvent(pubEv *pb.TapEvent_Http) *requestInitEvent {
	reqI := pubEv.GetRequestInit()
	if reqI != nil {
		sid := &streamID{
			Base:   reqI.GetId().GetBase(),
			Stream: reqI.GetId().GetStream(),
		}
		return &requestInitEvent{
			ID:        sid,
			Method:    reqI.GetMethod(),
			Scheme:    reqI.GetScheme(),
			Authority: reqI.GetAuthority(),
			Path:      reqI.GetPath(),
		}
	}
	return nil
}

// Attempt to map a `TapEvent_Http_ResponseInit` event to a `responseInitEvent`
func getResponseInitEvent(pubEv *pb.TapEvent_Http) *responseInitEvent {
	resI := pubEv.GetResponseInit()
	if resI != nil {
		sid := &streamID{
			Base:   resI.GetId().GetBase(),
			Stream: resI.GetId().GetStream(),
		}
		return &responseInitEvent{
			ID:               sid,
			SinceRequestInit: resI.GetSinceRequestInit(),
			HTTPStatus:       resI.GetHttpStatus(),
		}
	}
	return nil
}

// Attempt to map a `TapEvent_Http_ResponseEnd` event to a `responseEndEvent`
func getResponseEndEvent(pubEv *pb.TapEvent_Http) *responseEndEvent {
	resE := pubEv.GetResponseEnd()
	if resE != nil {
		sid := &streamID{
			Base:   resE.GetId().GetBase(),
			Stream: resE.GetId().GetStream(),
		}
		return &responseEndEvent{
			ID:                sid,
			SinceRequestInit:  resE.GetSinceRequestInit(),
			SinceResponseInit: resE.GetSinceResponseInit(),
			ResponseBytes:     resE.GetResponseBytes(),
			GrpcStatusCode:    resE.GetEos().GetGrpcStatusCode(),
			ResetErrorCode:    resE.GetEos().GetResetErrorCode(),
		}
	}
	return nil
}
