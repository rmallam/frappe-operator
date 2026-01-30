# Frappe Operator on OpenShift (CRC) Demo

This guide sets up a complete Frappe environment on OpenShift Local (CRC) using the Frappe Operator and MariaDB Operator.

## 1. Setup Helm Repositories

Add the required Helm repositories for the operators.

```bash
helm repo add mariadb-operator https://mariadb-operator.github.io/mariadb-operator
helm repo add frappe-operator https://rmallam.github.io/frappe-operator/helm-repo
helm repo update
```

## 2. Install Operators

### MariaDB Operator
Install the MariaDB operator with CRDs enabled.

```bash
helm upgrade --install mariadb-operator mariadb-operator/mariadb-operator \
  --namespace frappe-operator-system \
  --set crds.enabled=true \
  --create-namespace
```

### Frappe Operator
Install the Frappe Operator (disabling KEDA for this simple demo).

```bash
helm upgrade --install frappe-operator frappe-operator/frappe-operator \
  --namespace frappe-operator-system \
  --set keda.enabled=false \
  --create-namespace
```

## 3. Deploy Database (MariaDB)

Create the `mariadb` namespace, root credentials, and the database instance.

```yaml
apiVersion: project.openshift.io/v1
kind: Project
metadata:
  name: mariadb
---
apiVersion: v1
kind: Secret
metadata:
  name: mariadb-root-password
  namespace: mariadb
type: Opaque
stringData:
  password: frappe
---
apiVersion: k8s.mariadb.com/v1alpha1
kind: MariaDB
metadata:
  name: frappe-mariadb
  namespace: mariadb
spec:
  rootPasswordSecretKeyRef:
    name: mariadb-root-password
    key: password
  image: mariadb:10.11
  storage:
    size: 2Gi
  replicas: 1
```

## 4. Deploy Frappe Bench & Site

Create the `frappe` namespace, the Bench (cluster), and the Site.

> **Note:** This configuration uses the default CRC storage class (`crc-csi-hostpath-provisioner`). For production-grade or scalable setups, ensure you use a **ReadWriteMany (RWX)** capable storage class (like `openebs-kernel-nfs`) for the `sites` volume.

```yaml
apiVersion: project.openshift.io/v1
kind: Project
metadata:
  name: frappe
---
apiVersion: vyogo.tech/v1alpha1
kind: FrappeBench
metadata:
  name: bench-test
  namespace: frappe
spec:
  frappeVersion: "15.0.0"
  imageConfig:
    repository: ghcr.io/rmallam/frappe_docker
    tag: sha-7bc7484
    pullPolicy: Always
  storageClassName: crc-csi-hostpath-provisioner
  storageSize: 2Gi

  # Apps to install
  apps:
    - name: frappe
      source: image
    - name: erpnext
      source: image

  # Component replicas (minimal for development)
  componentReplicas:
    gunicorn: 1
    nginx: 1
    socketio: 1
    scheduler: 0
    workers:
      default: 1
      short: 0
      long: 0
---
apiVersion: vyogo.tech/v1alpha1
kind: FrappeSite
metadata:
  name: vyogotechdemo
  namespace: frappe
spec:
  siteName: demo.vyogo.apps-crc.testing
  benchRef:
    name: bench-test
    namespace: frappe

  # Database configuration
  dbConfig:
    provider: mariadb
    mode: shared
    mariadbRef:
      name: frappe-mariadb
      namespace: mariadb

  # Ingress configuration for OpenShift
  ingress:
    enabled: true
    className: openshift-default
```