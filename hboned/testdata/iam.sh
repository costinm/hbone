PROJECT_ID=${PROJECT_ID:-costin-asm1}
CONFIG_PROJECT_ID=${CONFIG_PROJECT_ID:-costin-asm1}
WORKLOAD_NAMESPACE=${WORKLOAD_NAMESPACE:-default}

function initGSA() {

  gcloud --project ${PROJECT_ID} iam service-accounts create k8s-${WORKLOAD_NAMESPACE} \
        --display-name "Service account with access to ${WORKLOAD_NAMESPACE} k8s namespace" || true



  # Workload identity - MDS can also impersonate the GSA
  gcloud iam service-accounts add-iam-policy-binding \
      --role roles/iam.workloadIdentityUser \
      --member "serviceAccount:${CONFIG_PROJECT_ID}.svc.id.goog[${WORKLOAD_NAMESPACE}/default]" \
      k8s-${WORKLOAD_NAMESPACE}@${PROJECT_ID}.iam.gserviceaccount.com

  # K8S side of the mapping.
  kubectl annotate serviceaccount \
          --namespace ${WORKLOAD_NAMESPACE} default \
          iam.gke.io/gcp-service-account=k8s-${WORKLOAD_NAME}@${PROJECT_ID}.iam.gserviceaccount.com

}
# ========= Grant the GCP equivalent SA permissions =============

function grantRole() {
  local r=$1

  gcloud  projects add-iam-policy-binding ${PROJECT_ID}  \
      --member="serviceAccount:k8s-${WORKLOAD_NAMESPACE}@${PROJECT_ID}.iam.gserviceaccount.com" \
      --role="roles/${r}"
}

function grantServiceRole() {
  local r=$1

  gcloud run services add-iam-policy-binding ${SERVICE} \
      --member="serviceAccount:k8s-${WORKLOAD_NAMESPACE}@${PROJECT_ID}.iam.gserviceaccount.com" \
      --role="roles/${r}"
}

function grantRoles() {
  #grantServiceRole run.invoker

  # Also allow the use of TD
  grantRole  trafficdirector.client
  # This allows the GSA to use the GKE and other APIs in the 'config cluster' project.
  grantRole  serviceusage.serviceUsageConsumer
  # Grant the GSA running the workload permission to connect to the config clusters in the config project.
  # Will use the 'SetQuotaProject' - otherwise the GKE API must be enabled in the workload project.
  grantRole container.clusterViewer

  # Connect to hub connector, view hub
  grantRole gkehub.connect

  grantRole gkehub.viewer

}

initGSA
