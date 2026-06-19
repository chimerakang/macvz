# MacVz — Apple Silicon Kubernetes 節點供應器

*[English](README.md) | 繁體中文*

**MacVz 讓 Apple Silicon (M 系列) Mac 成為一等公民的 Kubernetes 節點，並以原生 micro-VM 形式執行 OCI 工作負載。**

我們不打算再造一個編排器，而是透過 [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) 介面，直接接上**標準的 Kubernetes 控制平面**。每台 Mac mini 以一個虛擬節點 (virtual node) 的身分加入叢集；每個被排程到該節點的 Pod，都會經由 Apple 原生的 **Virtualization.framework** 啟動成一個獨立的 Linux micro-VM——底層運行時採用 [`apple/container`](https://github.com/apple/container)。最終達成：強隔離、秒級啟動、低功耗，並完全釋放統一記憶體頻寬，且不必背負 Docker Desktop 那套巨型 Linux VM。

> **定位：** MacVz **不是**一個新的控制平面，而是一個**節點層 (node-layer)** 專案。你自備（或自行架設）一套正常的 Kubernetes 叢集；MacVz 負責讓 Apple Silicon 主機可被當成節點使用。所有價值都集中在「運行時整合」與「跨主機網路」這兩塊——也正是目前生態系尚未提供的部分。

---

## 1. 核心設計原則

- **站在 Kubernetes 之上，而非取代它。** 透過 Virtual Kubelet 直接繼承 `kubectl`、調度器、宣告式 API、Service、RBAC 與整個生態系。不自建控制平面，Mac 上也不需營運 etcd。
- **Micro-VM 隔離。** 一 Pod = 一個獨立、極簡的 Linux micro-VM，工作負載之間不共享 guest kernel。
- **善用 Apple 的運行時。** 直接採用 `apple/container` 處理映像拉取、guest kernel、RootFS 與 VM 內 init shim，不重複實作最困難的底層虛擬化工程。
- **Go 原生黏合層。** Provider、runtime driver 與網路層皆以 Go 撰寫；唯一的非 Go 依賴是 Apple 的運行時，透過其 CLI／服務 API 驅動。
- **扁平化跨主機網路。** 以 WireGuard 網格提供跨 Mac 的 Pod 之間直接、加密的 L3 連通性。

---

## 2. 系統架構

```
        ┌────────────────────────────────────────────┐
        │           標準 Kubernetes 控制平面            │
        │  (api-server / scheduler / etcd — 單台主機)  │   ← 由你架設，原封不動
        └───────────────┬──────────────────────────────┘
                        │ kubelet API (Virtual Kubelet)
      ┌─────────────────┼──────────────────────┐
      ▼                 ▼                       ▼
┌───────────┐    ┌───────────┐           ┌───────────┐
│ Mac mini  │    │ Mac mini  │   ...     │ Mac mini  │   ← 每台 = 一個虛擬節點
│ macvz-    │    │ macvz-    │           │ macvz-    │
│ kubelet   │    │ kubelet   │           │ kubelet   │
│  ├ provider (Virtual Kubelet)          │           │
│  ├ runtime  (apple/container 驅動)      │           │
│  └ network  (WireGuard 網格)            │           │
│   micro-VM  micro-VM  micro-VM          │  micro-VM │
└───────────┘    └───────────┘           └───────────┘
        └──────── WireGuard 加密網格 ────────────────┘
```

### 你「不需要」自己做的部分

- **Kubernetes 控制平面** —— 採用任何標準發行版即可（單節點 `k3s`、`k0s`，或完整 `kubeadm` 叢集皆可）。etcd、調度器與 API server 全部原樣沿用，且可集中跑在一台機器上（Mac 或 Linux 皆可）。

### MacVz 提供的部分（`macvz-kubelet`，每台 Mac 常駐一個行程）

- **Provider（Virtual Kubelet）** —— 將 Mac 註冊為節點，向調度器回報 CPU/RAM 容量，並實作 Pod 生命週期：`CreatePod`、`UpdatePod`、`DeletePod`、`GetPod(s)`、`GetPodStatus`，以及 `GetContainerLogs`、`RunInContainer`（exec）與 metrics。
- **Runtime 驅動層** —— 將 Kubernetes Pod spec 翻譯成 `apple/container` 操作（拉取 OCI 映像 → 啟動 micro-VM → 設定 env／command／掛載 → 串流日誌）。這是核心黏合層。
- **網路層** —— 為每個 Pod 分配叢集 IP，並透過 WireGuard 網格讓跨 Mac 的 Pod 直接互通；同時把 Pod IP 回報給 Kubernetes，讓 Service／Endpoints 正常運作。

---

## 3. 技術棧 (Tech Stack)

| 分層 | 採用技術 / 開源庫 | 說明 |
| --- | --- | --- |
| 核心語言 | Go (Golang) | Provider、runtime 驅動與網路層；與 `client-go` 整合順暢。 |
| 節點整合 | `virtual-kubelet/virtual-kubelet` | 無須在 macOS 上跑真正的 kubelet/CRI，即可把 Mac 呈現為 Kubernetes 節點。 |
| 容器運行時 | [`apple/container`](https://github.com/apple/container) | Apache-2.0 的 Apple 運行時：OCI 映像拉取、guest kernel、RootFS、VM 內 init (`vminitd`)、Apple Silicon 上秒級 micro-VM 啟動。 |
| macOS 虛擬化 | Virtualization.framework（經由 `apple/container`） | Apple Silicon 原生 hypervisor，不依賴第三方 VMM。 |
| Kubernetes 客戶端 | `k8s.io/client-go` | 與 API server 溝通、watch Pod、回報 node/Pod 狀態。 |
| 跨主機網路 | WireGuard（Go 原生實現） | 加密 P2P 網格，提供跨 Mac 的 Pod 扁平化 L3 連通性（CNI 對應層）。 |
| 組態解析 | go-yaml | Provider／節點組態。 |

> **參考專案：** [`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet) 是 macOS 上 Virtual Kubelet 路線最接近的前行者。[`abiosoft/colima`](https://github.com/abiosoft/colima) 在 CLI/UX 設計，以及「Go 程式如何驅動 Apple `vz` 後端」上具參考價值——但**不要**參考它的 Kubernetes 模型（它是在單一大型 VM 內跑 k3s，與本設計理念完全相反）。

---

## 4. 專案目錄結構（標準 Go 佈局）

```
macvz/
├── cmd/
│   └── macvz-kubelet/        # Virtual Kubelet provider 主程式（每台 Mac 一個）
│       └── main.go
├── pkg/
│   ├── provider/             # Virtual Kubelet PodLifecycleHandler 實作
│   ├── runtime/              # apple/container 整合（CLI／服務 API 驅動）
│   ├── network/              # WireGuard 網格 + Pod IPAM + IP 回報
│   ├── config/               # YAML 組態解析
│   └── metrics/              # 向 Kubernetes 回報 node 與 pod 資源
├── deployments/              # 範例 k8s manifest、RBAC、節點啟動腳本
├── go.mod
└── README.md
```

---

## 5. 漸進式開發階段規劃

> **設計心法：** 先在單台 Mac 上把 runtime 層玩通，再讓它成為 Kubernetes 節點，最後串起跨機網路。

### 階段一：Runtime 整合（單機，先不接 Kubernetes）

**目標：** 用 Go 驅動 `apple/container`，完整管理一個 micro-VM 的生命週期。

- 初始化專案架構與 `go.mod`。
- 建立 `pkg/runtime`：透過 `apple/container` 的 CLI／服務 API，從 Go 拉取 OCI 映像、秒級啟動一個 Alpine micro-VM、停止／銷毀、串流日誌、exec 進入容器。
- 定義 provider 之後要依附的抽象介面（start/stop/status/logs/exec）。

### 階段二：Virtual Kubelet Provider MVP

**目標：** 單台 Mac 出現在 `kubectl get nodes` 中，並能執行真實 Pod。

- 建立 `pkg/provider`，實作 Virtual Kubelet 的 `PodLifecycleHandler`。
- 將 Mac 註冊為虛擬節點；回報 CPU/RAM 容量，讓標準調度器能派 Pod。
- 將 Pod spec 翻譯成 `pkg/runtime` 呼叫（映像、command/args、env、資源限制）。
- 打通 `kubectl logs` 與 `kubectl exec`。
- **驗收：** `kubectl run alpine --image=alpine --restart=Never -- sleep 3600` 能在 Mac 上落地一個 micro-VM，且 `kubectl logs`／`exec` 正常。

### 階段三：跨主機網格網路

**目標：** Mac A 上的 Pod 能連到 Mac B 上的 Pod；Service 能在全叢集解析。

- 實作經由 Kubernetes 協調的 Pod IPAM（不採去中心化自分配，避免 IP 衝突）。
- 在 Mac 之間建立 WireGuard 網格；以使用者態網路路徑（例如 file-handle attachment + gvisor-tap-vsock 風格的堆疊）把 micro-VM 流量導入 WireGuard 介面，使叢集 IP 完全可控。
- 把 Pod IP 回報給 API server，讓 Endpoints／Service 正常運作；新增 port-forward 支援。
- **驗收：** 一個由分屬兩台 Mac 的 Pod 支撐的 Service，能透過正常的 Kubernetes 網路被存取。

---

## 6. 環境需求與已知限制

- **macOS 26 (Tahoe) 以上、Apple Silicon。** `apple/container` 要求此環境；容器間與主機網路依賴較新的 OS 支援。
- **`apple/container` 是硬依賴**（Apache-2.0，pre-1.0）。1.0 前 API 可能變動；請鎖定版本，並把所有呼叫隔離在 `pkg/runtime` 內。
- **密度受限於 RAM，而非容器式的 kernel 共享。** 每個 micro-VM 自帶 kernel 與固定記憶體下限。請在階段一就驗證單機實際的並發 VM 上限與每 VM 開銷——這決定專案的實用容量。
- **映像架構。** Guest 為 arm64。拉取 amd64 映像需使用 arm64 variant 或 Rosetta-for-Linux 支援；應向使用者明確說明。
- **安全。** `macvz-kubelet` ↔ API server 的通道必須使用叢集既有的 mTLS/RBAC。不要對外公開 runtime 服務或節點埠口。映像倉庫憑證與任何機密均來自 Kubernetes Secrets／環境變數，絕不硬編碼。
- **Pod `securityContext` 模型。** 每個 Pod 都是獨立 micro-VM（自己的 kernel、硬體隔離），邊界比共享 kernel 的容器更強。MacVz 會把 runtime 能強制執行的欄位映射到 `apple/container`（`runAsUser`／`runAsGroup` → `--user`、`readOnlyRootFilesystem` → `--read-only`、`capabilities` → `--cap-add`／`--cap-drop`），接受已由 VM 邊界滿足的欄位（例如 `allowPrivilegeEscalation`、`seccomp`／`appArmor` 的 `RuntimeDefault`、`fsGroup`），並對無法履行的欄位回報終止性的 `Failed` 狀態，而不是靜默略過（`privileged: true`、`seLinuxOptions`、`Localhost` seccomp／appArmor、`procMount`、`sysctls`）。`runAsNonRoot` 只有搭配 `runAsUser` 時才會被明確驗證。完整表格見 [docs/WORKLOADS.md](docs/WORKLOADS.md#securitycontext-52)。
- **特權網路需要 root 工具，但 kubelet 以你的使用者身分執行。** 跨 Mac 資料平面（WireGuard 網格 + pf／route／sysctl）需要 root，但 `apple/container` 拒絕以 root 執行——因此 `macvz-kubelet` 以使用者身分執行，並透過 unix socket 將特權命令委派給 `macvz-netd` 輔助常駐程式。輔助程式只需用 `sudo` 安裝一次；日常啟動 kubelet 不需提權。完整的安裝與復原手冊見 [docs/PRIVILEGED_NETWORKING.md](docs/PRIVILEGED_NETWORKING.md)。
- **營運多節點叢集。** 將一台 Mac 節點加入、驗證、drain、移除、升級、排錯與清理，統整為單一生命週期手冊：[docs/MULTI_NODE_OPS.md](docs/MULTI_NODE_OPS.md)。這份文件是操作順序索引，把節點加入、WireGuard 網格、特權網路復原程序，以及即時 `/healthz/diagnostics` 健康報告串接在一起。
- **Entitlement 與簽章。** `macvz-kubelet` 以常駐行程執行，需要虛擬化 entitlement，並須正確簽章（對外分發還需 notarization）。
