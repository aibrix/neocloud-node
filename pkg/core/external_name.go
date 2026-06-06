package core

import "strings"

const ManagedExternalNamePrefix = "gpu-burst-node-"

func ManagedExternalName(idempotencyKey string) string {
	name := strings.ToLower(idempotencyKey)
	replacer := strings.NewReplacer("/", "-", "_", "-", ".", "-", ":", "-")
	name = replacer.Replace(name)
	return ManagedExternalNamePrefix + name
}

func IsManagedExternalName(name string) bool {
	return strings.HasPrefix(name, ManagedExternalNamePrefix)
}
