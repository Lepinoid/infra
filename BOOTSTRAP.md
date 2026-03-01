# テナントセットアップガイド

このガイドは、turtton/infra クラスタにテナントとしてワークロードをデプロイするための手順を説明する。

## 1. 前提条件

### SOPS + Age 鍵ペアの生成

テナント側で独自のAge鍵ペアを生成し、Kubernetes Secretの暗号化に使用する。

```bash
# Age鍵ペア生成
age-keygen -o age.key
# 公開鍵が標準出力に表示される（例: age1xxxxxxxxxx...）
```

生成された秘密鍵ファイル（`age.key`）を管理者に安全な手段で渡す。管理者がクラスタに登録する。

```bash
# 管理者が実行: テナントのAge秘密鍵をクラスタに登録
kubectl create secret generic sops-age-<テナント名> \
  --namespace=flux-system \
  --from-file=age.agekey=<テナント名>-age.key
```

### リポジトリの `.sops.yaml` 設定

リポジトリルートに `.sops.yaml` を作成する。`age` フィールドには生成した公開鍵を設定する。

```yaml
creation_rules:
  - path_regex: \.sops\.(yaml|yml)$
    encrypted_regex: ^(data|stringData)$
    age: "age1xxxxxxxxxx..."  # 生成した公開鍵
```

## 2. リポジトリ構成例

以下はlepinoidテナントの構成例。

```
<テナントリポジトリ>/
├── .sops.yaml
├── kustomization.yaml          # 全リソースを参照
├── couchdb/
│   ├── credentials.sops.yaml   # SOPS暗号化Secret
│   ├── configmap.yaml          # CouchDB設定
│   ├── pvc.yaml                # PVC (Longhorn)
│   ├── deployment.yaml         # CouchDB Deployment
│   └── service.yaml            # ClusterIP Service
├── livesync-bridge/
│   ├── config.sops.yaml        # bridge設定Secret
│   ├── scripts.yaml            # bridgeスクリプトConfigMap
│   └── deployment.yaml         # bridge Deployment
├── cloudflared/
│   ├── tunnel-token.sops.yaml  # トンネルトークンSecret
│   ├── network-policy.yaml     # cloudflared用egress許可
│   └── deployment.yaml         # cloudflared Deployment
└── grafana/
    ├── admin-credentials.sops.yaml
    ├── datasource.yaml         # ConfigMap: Prometheus URL
    ├── pvc.yaml                # Grafanaデータ用PVC
    ├── deployment.yaml         # Grafana Deployment
    └── service.yaml            # ClusterIP Service
```

ルートの `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - couchdb/credentials.sops.yaml
  - couchdb/configmap.yaml
  - couchdb/pvc.yaml
  - couchdb/deployment.yaml
  - couchdb/service.yaml
  - livesync-bridge/config.sops.yaml
  - livesync-bridge/scripts.yaml
  - livesync-bridge/deployment.yaml
  - cloudflared/tunnel-token.sops.yaml
  - cloudflared/network-policy.yaml
  - cloudflared/deployment.yaml
  - grafana/admin-credentials.sops.yaml
  - grafana/datasource.yaml
  - grafana/pvc.yaml
  - grafana/deployment.yaml
  - grafana/service.yaml
```

## 3. 各コンポーネントの設定ガイド

### 共通注意事項

- **`metadata.namespace` は省略すること** — Flux の `targetNamespace` により自動設定される。明示的に指定すると競合する可能性がある
- **全コンテナに `resources.requests` / `resources.limits` を設定すること** — ResourceQuotaが設定されているため、未設定のPodは作成が拒否される
- **`nodeSelector` は省略すること** — テナントのスケジューリングはResourceQuotaで制約される

### CouchDB

turtton/infra の `clusters/main/apps/obsidian-livesync/` を参考に構成する。

**credentials.sops.yaml** (暗号化前):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: couchdb-credentials
type: Opaque
stringData:
  COUCHDB_USER: admin
  COUCHDB_PASSWORD: <パスワード>
```

暗号化: `sops --encrypt --in-place couchdb/credentials.sops.yaml`

**pvc.yaml**:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: couchdb-data
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: longhorn
  resources:
    requests:
      storage: 500Mi
```

**deployment.yaml** のリソース設定例:

```yaml
resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    memory: 512Mi
```

### LiveSync Bridge

turtton/infra の `clusters/main/apps/obsidian-livesync/livesync-bridge-*` を参考に構成する。

リソース設定例:

```yaml
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    memory: 128Mi
```

### Cloudflare Tunnel

テナント独自のCloudflareアカウントおよびトンネルを使用する。turtton/infra の `clusters/main/infrastructure/controllers/cloudflared/` を参考に構成する。

**tunnel-token.sops.yaml** (暗号化前):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cloudflared-tunnel-token
type: Opaque
stringData:
  token: <Cloudflare Tunnelトークン>
```

**network-policy.yaml** — cloudflared用の外部Egress許可。base テンプレートの default-deny により外部通信がブロックされるため、cloudflared Podには明示的な許可が必要:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-cloudflared-egress
spec:
  podSelector:
    matchLabels:
      app: cloudflared
  policyTypes:
    - Egress
  egress:
    - {}
```

**deployment.yaml** のリソース設定例:

```yaml
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    memory: 128Mi
```

### Grafana（スタンドアロン）

既存のPrometheusに接続するスタンドアロンGrafanaをデプロイする。

**datasource.yaml**:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-datasources
data:
  datasources.yaml: |
    apiVersion: 1
    datasources:
      - name: Prometheus
        type: prometheus
        access: proxy
        url: http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090
        isDefault: true
```

**admin-credentials.sops.yaml** (暗号化前):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: grafana-admin-credentials
type: Opaque
stringData:
  admin-user: admin
  admin-password: <パスワード>
```

**deployment.yaml** の例:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: grafana
spec:
  replicas: 1
  selector:
    matchLabels:
      app: grafana
  template:
    metadata:
      labels:
        app: grafana
    spec:
      containers:
        - name: grafana
          image: grafana/grafana:11.5.2
          env:
            - name: GF_SECURITY_ADMIN_USER
              valueFrom:
                secretKeyRef:
                  name: grafana-admin-credentials
                  key: admin-user
            - name: GF_SECURITY_ADMIN_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: grafana-admin-credentials
                  key: admin-password
          ports:
            - name: http
              containerPort: 3000
              protocol: TCP
          volumeMounts:
            - name: grafana-data
              mountPath: /var/lib/grafana
            - name: grafana-datasources
              mountPath: /etc/grafana/provisioning/datasources
              readOnly: true
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 256Mi
      volumes:
        - name: grafana-data
          persistentVolumeClaim:
            claimName: grafana-data
        - name: grafana-datasources
          configMap:
            name: grafana-datasources
```

**service.yaml**:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: grafana
spec:
  selector:
    app: grafana
  ports:
    - port: 80
      targetPort: http
      protocol: TCP
```

Cloudflare Tunnel経由でアクセス可能にする場合は、cloudflaredのトンネル設定でこのServiceをルーティング先に指定する。

推奨ダッシュボード: Kubernetes namespace overview

## 4. 制約事項

| 項目 | 制限値 |
|------|--------|
| requests.cpu | 1 |
| requests.memory | 2Gi |
| limits.cpu | 2 |
| limits.memory | 4Gi |
| PVC数 | 5 |

- **クラスタスコープのリソースは作成不可** — ClusterRole, CustomResourceDefinition, PersistentVolume 等
- **全Podに `resources.requests` / `resources.limits` を設定すること** — 未設定のPodはResourceQuotaにより作成が拒否される
- **`metadata.namespace` は省略推奨** — Flux の `targetNamespace` で自動設定される

## 5. デプロイ手順

1. テナントリポジトリをpublic化する（またはFlux GitRepositoryの認証を設定する）
2. マニフェストを作成し、commit & push する
3. 管理者にSOPS秘密鍵（`age.key`）を安全な手段で渡す
4. 管理者がクラスタにAge秘密鍵Secretを登録する:
   ```bash
   kubectl create secret generic sops-age-<テナント名> \
     --namespace=flux-system \
     --from-file=age.agekey=<テナント名>-age.key
   ```
5. 管理者が `lepinoid-workloads` Kustomization の `suspend: false` に変更してcommit
6. Flux が自動的にreconcileし、テナントのワークロードがデプロイされる

## 6. 検証

デプロイ後、管理者側で以下のコマンドで状態を確認できる。

```bash
# Flux Kustomization の状態確認
flux get kustomizations tenants
flux get kustomizations lepinoid-tenant-setup
flux get kustomizations lepinoid-workloads

# GitRepository の fetch 状態確認
flux get sources git lepinoid-infra

# テナントリソースの確認
kubectl get namespace lepinoid
kubectl get serviceaccount -n lepinoid lepinoid-deployer
kubectl get rolebinding -n lepinoid lepinoid-admin
kubectl get resourcequota -n lepinoid
kubectl get networkpolicy -n lepinoid

# テナントワークロードの確認
kubectl get all -n lepinoid
```

## 7. 注意事項

- NetworkPolicy は現在のFlannel CNIでは強制されない。Cilium移行後に自動的に有効になる
- Cilium移行時、テナント固有のegress（cloudflared等）はテナントリポジトリ側のNetworkPolicyで許可する必要がある
- `lepinoid-workloads` は初期状態で `suspend: true`。テナントリポジトリとSOPS鍵が準備できてから解除する
- テナントリポジトリがprivateの場合、GitRepositoryのfetchが認証エラーで失敗する
