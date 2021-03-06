/*
Copyright 2016 The Fission Authors.

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

package portforward

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/fission/fission/fission/log"
)

// Port forward a free local port to a pod on the cluster. The pod is
// found in the specified namespace by labelSelector. The pod's port
// is found by looking for a service in the same namespace and using
// its targetPort. Once the port forward is started, wait for it to
// start accepting connections before returning.
func Setup(kubeConfig, namespace, labelSelector string) string {
	log.Verbose(2, "Setting up port forward to %s in namespace %s using the kubeconfig at %s",
		labelSelector, namespace, kubeConfig)

	localPort, err := findFreePort()
	if err != nil {
		log.Fatal(fmt.Sprintf("Error finding unused port :%v", err.Error()))
	}

	log.Verbose(2, "Waiting for local port %v", localPort)
	for {
		conn, _ := net.DialTimeout("tcp",
			net.JoinHostPort("", localPort), time.Millisecond)
		if conn != nil {
			conn.Close()
		} else {
			break
		}
		time.Sleep(time.Millisecond * 50)
	}

	log.Verbose(2, "Starting port forward from local port %v", localPort)
	go func() {
		err := runPortForward(kubeConfig, labelSelector, localPort, namespace)
		if err != nil {
			log.Fatal(fmt.Sprintf("Error forwarding to controller port: %s", err.Error()))
		}
	}()

	log.Verbose(2, "Waiting for port forward %v to start...", localPort)
	for {
		conn, _ := net.DialTimeout("tcp",
			net.JoinHostPort("", localPort), time.Millisecond)
		if conn != nil {
			conn.Close()
			break
		}
		time.Sleep(time.Millisecond * 50)
	}

	log.Verbose(2, "Port forward from local port %v started", localPort)

	return localPort
}

func findFreePort() (string, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return "", err
	}

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	err = listener.Close()
	if err != nil {
		return "", err
	}

	return port, nil
}

// runPortForward creates a local port forward to the specified pod
func runPortForward(kubeConfig string, labelSelector string, localPort string, ns string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		log.Fatal(fmt.Sprintf("Failed to connect to Kubernetes: %s", err))
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(fmt.Sprintf("Failed to connect to Kubernetes: %s", err))
	}

	log.Verbose(2, "Connected to Kubernetes API")

	// if namespace is unset, try to find a pod in any namespace
	if len(ns) == 0 {
		ns = meta_v1.NamespaceAll
	}

	// get the pod; if there is more than one, ask the user to disambiguate
	podList, err := clientset.CoreV1().Pods(ns).
		List(meta_v1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(podList.Items) == 0 {
		log.Fatal("Error getting controller pod for port-forwarding")
	}

	// make a useful error message if there is more than one install
	if len(podList.Items) > 1 {
		namespaces := make([]string, 0)
		for _, p := range podList.Items {
			namespaces = append(namespaces, p.Namespace)
		}
		log.Fatal(fmt.Sprintf("Found %v fission installs, set FISSION_NAMESPACE to one of: %v",
			len(podList.Items), strings.Join(namespaces, " ")))
	}

	// pick the first pod
	podName := podList.Items[0].Name
	podNameSpace := podList.Items[0].Namespace

	// get the service and the target port
	svcs, err := clientset.CoreV1().Services(podNameSpace).
		List(meta_v1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		log.Fatal(fmt.Sprintf("Error getting %v service :%v", labelSelector, err.Error()))
	}
	if len(svcs.Items) == 0 {
		log.Fatal(fmt.Sprintf("Service %v not found", labelSelector))
	}
	service := &svcs.Items[0]

	var targetPort string
	for _, servicePort := range service.Spec.Ports {
		targetPort = servicePort.TargetPort.String()
	}
	log.Verbose(2, "Connecting to port %v on pod %v/%v", targetPort, podNameSpace, podNameSpace)

	stopChannel := make(chan struct{}, 1)
	readyChannel := make(chan struct{})

	// create request URL
	req := clientset.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(podNameSpace).Name(podName).SubResource("portforward")
	url := req.URL()

	// create ports slice
	portCombo := localPort + ":" + targetPort
	ports := []string{portCombo}

	// actually start the port-forwarding process here
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		msg := fmt.Sprintf("Failed to connect to Fission service on Kubernetes: %v", err.Error())
		log.Fatal(msg)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)

	outStream := os.Stdout
	if log.Verbosity < 2 {
		outStream = nil
	}
	fw, err := portforward.New(dialer, ports, stopChannel, readyChannel, outStream, os.Stderr)
	if err != nil {
		msg := fmt.Sprintf("portforward.new errored out :%v", err.Error())
		log.Fatal(msg)
	}

	log.Verbose(2, "Starting port forwarder")
	return fw.ForwardPorts()
}
