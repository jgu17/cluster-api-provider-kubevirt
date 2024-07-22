package util

func CreateClusterServiceAccountSecretName(serviceAccountName string) string {
	return serviceAccountName + "-token"
}
