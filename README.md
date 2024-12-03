# KGrok

KGrok provides an easy way to expose a developer's local machine as an HTTPS endpoint to the web.
Once set up, it uses Kubernetes and the Gateway API to let developers define subdomain endpoints and
tunnel traffic to their machines with a single command.

This project originated from my need to handle a GitHub webhook directly from my machine.
Over time, it evolved into a more general-purpose solution, addressing a common developer experience challenge:
How to grant dynamic HTTPS endpoint to my developers without giving up on security and ease of use.

## How It Works

KGrok operates in two phases:

1. **Configuration Phase**: Configure your Kubernetes cluster to enable developer self-service.
This includes setting up DNS and provisioning an API Gateway with a wildcard certificate for your subdomain.

2. **Self-Service Phase**: Developers use the KGrok binary to create a tunnel deployment and route.
Once provisioned, KGrok port-forwards traffic between the local machine and the deployment (using [ktunnel][ktunnel] under the hood),
exposing local ports to the Internet.

## Getting Started

### Prerequisites

1. **Kubernetes Cluster**: Ensure you have a Kubernetes cluster set up.
2. **Domain Name**: A valid domain name is required.
3. **Ingress with Gateway API Support**: Install a Gateway API-compatible ingress controller.
For example, [Contour][contour] can be installed by following [these instructions][contour_install].
4. **A `cert-manager` deployment**: You can follow [the official documentation][certmanager_install].
5. **DNS Configuration**: Set a `CNAME` entry for the subdomain. For instance, `*.staging.example.com` should point to your cluster.

**Important**: Avoid using a wildcard certificate for the top-level domain (e.g., `*.example.com`) due to potential security risks.

### Setting Up Certificates

1. Configure the DNS challenge for Let's Encrypt in `cert-manager` using DigitalOcean.
   See [this guide][dns_challenge] for details.
2. Create a `ClusterIssuer` for Let's Encrypt that uses the configured DNS challenge:

    ```yaml
    apiVersion: cert-manager.io/v1
    kind: ClusterIssuer
    metadata:
      name: letsencrypt
    spec:
      acme:
        privateKeySecretRef:
          name: letsencrypt
        server: https://acme-v02.api.letsencrypt.org/directory
        solvers:
        - dns01:
          digitalocean:
            tokenSecretRef:
              key: access-token
              name: digital-ocean-dns
          selector:
            dnsZones:
            - example.com
    ```

3. Create a wildcard `Certificate` object for your subdomain.

    ```yaml
    apiVersion: cert-manager.io/v1
    kind: Certificate
    metadata:
      name: example-staging
      namespace: contour
    spec:
      commonName: '*.staging.example.com'
      dnsNames:
        - '*.staging.example.com'
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt
      secretName: example-staging
    ```

### Setting Up a Gateway

Create or update your Gateway resource in the cluster to handle traffic and use the certificate to terminate TLS:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: contour
  namespace: contour
spec:
  gatewayClassName: contour
  listeners:
    - allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              environment: staging
      hostname: '*.staging.example.com'
      name: example-staging
      port: 443
      protocol: HTTPS
      tls:
        mode: Terminate
        certificateRefs:
          - kind: Secret
            name: example-staging
```

Namespaces labeled with `environment: staging` can now create `HTTPRoute` objects for subdomains.

### Use KGrok to expose your local ports behind an HTTPS endpoint

KGrok can deploy the necessary resources (Pod, Service, and HTTPRoute) with a single command. Here’s an example template:

```yaml
cat <<EOF | kgrok 8080
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: foo-staging
spec:
  hostnames:
    - foo.staging.example.com
  parentRefs:
    - name: contour
      namespace: contour
      sectionName: example-staging
  rules:
    - backendRefs:
        - kind: Service
          name: {{.Name}}
          port: 8081
---
apiVersion: v1
kind: Service
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  selector:
    app: {{.Name}}
  ports:
    - port: 8081
      targetPort: 8080
---
apiVersion: v1
kind: Pod
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  containers:
    - command: ["/ktunnel/ktunnel", "server", "-p", "{{.Port}}"]
      image: docker.io/omrieival/ktunnel:v1.6.1
      name: {{.Name}}
EOF
```

You can rebuild the binary with a default template matching your specific configuration.
The tool is taking a template from stdin for flexibility reason, however, it can be verbose.
Do not hesitate to adapt to your needs.

## Additional Notes

- The CLI is designed to serve as a simple example of interacting with Kubernetes using the Go client. You can reference the code to see how `kubectl apply -f-` is emulated.
- KGrok was made possible thanks to the excellent work on the [ktunnel project][ktunnel]. This project adds additional functionality and simplifies its usage.
- I still use a lot of legacy code, some context cancelation are not propagated properly, this is work in progress.

[wildcard_certificate]: https://stackoverflow.com/questions/66051624/generate-wildcard-certificate-on-kubernetes-cluster-with-digitalocean-for-my-ngi
[go_client]: https://github.com/kubernetes/client-go
[contour]: https://projectcontour.io/
[contour_install]: https://projectcontour.io/docs/1.23/guides/gateway-api/
[certmanager_install]: https://cert-manager.io/docs/installation/helm/
[dns_challenge]: https://stackoverflow.com/questions/66051624/generate-wildcard-certificate-on-kubernetes-cluster-with-digitalocean-for-my-ngi#answer-66097094
[ktunnel]: https://github.com/omrikiei/ktunnel
