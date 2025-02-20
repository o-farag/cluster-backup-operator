/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	veleroapi "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func findSuffix(slice []string, val string) (int, bool) {
	for i, item := range slice {
		if strings.HasSuffix(val, item) {
			return i, true
		}
	}
	return -1, false
}

// find takes a slice and looks for an element in it. If found it will
// return it's key, otherwise it will return -1 and a bool of false.
func find(slice []string, val string) (int, bool) {
	for i, item := range slice {
		if item == val {
			return i, true
		}
	}
	return -1, false
}

func findValue(slice []string, val string) bool {

	_, ok := find(slice, val)

	return ok
}

//append unique value to a list
func appendUnique(slice []string, value string) []string {
	// check if the NS exists
	_, ok := find(slice, value)
	if !ok {
		slice = append(slice, value)
	}
	return slice
}

// min returns the smallest of x or y.
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// returns a valid name for the velero restore kubernetes resource
// by trimming the concatenated cluster restore and backup names
func getValidKsRestoreName(clusterRestoreName string, backupName string) string {
	//max name for ns or resources is 253 chars
	fullName := clusterRestoreName + "-" + backupName

	if len(fullName) > 252 {
		return fullName[:252]
	}
	return fullName
}

// Velero uses TimestampedName for backups using the follwoing format
// by setting the default backup name format based on the schedule
// fmt.Sprintf("%s-%s", s.Name, timestamp.Format("20060102150405"))
// this function parses Velero backupName and returns the timestamp
func getBackupTimestamp(backupName string) (time.Time, error) {
	timestampIndex := strings.LastIndex(backupName, "-")
	if timestampIndex != -1 {
		timestampStr := strings.Trim(backupName[timestampIndex:], "-")
		return time.Parse("20060102150405", timestampStr)
	}
	return time.Time{}, nil
}

// SortResourceType implements sort.Interface
type SortResourceType []ResourceType

func (a SortResourceType) Len() int           { return len(a) }
func (a SortResourceType) Less(i, j int) bool { return a[i] < a[j] }
func (a SortResourceType) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// check if we have a valid storage location object
func isValidStorageLocationDefined(
	veleroStorageLocations veleroapi.BackupStorageLocationList,
) (bool, string) {
	isValidStorageLocation := false
	veleroNamespace := ""
	for i := range veleroStorageLocations.Items {
		if veleroStorageLocations.Items[i].OwnerReferences != nil &&
			veleroStorageLocations.Items[i].Status.Phase == veleroapi.BackupStorageLocationPhaseAvailable {
			for _, ref := range veleroStorageLocations.Items[i].OwnerReferences {
				if ref.Kind != "" {
					isValidStorageLocation = true
					veleroNamespace = veleroStorageLocations.Items[i].Namespace
					break
				}
			}
		}
		if isValidStorageLocation {
			break
		}
	}
	return isValidStorageLocation, veleroNamespace
}

// having a resourceKind.resourceGroup string, return (resourceKind, resourceGroup)
func getResourceDetails(resourceName string) (string, string) {

	indexOfName := strings.Index(resourceName, ".")
	if indexOfName > -1 {
		return resourceName[:indexOfName], resourceName[indexOfName+1:]
	}

	return resourceName, ""
}

// retrurn the set of CRDs for a potential generic resource,
// backed up by acm-resources-generic-schedule
// labeled by cluster.open-cluster-management.io/backup
func getGenericCRDFromAPIGroups(
	ctx context.Context,
	dc discovery.DiscoveryInterface,
	veleroBackup *veleroapi.Backup,
) ([]string, error) {

	logger := log.FromContext(ctx)

	resources := []string{}

	groupList, err := dc.ServerGroups()
	if err != nil {
		return resources, fmt.Errorf("failed to get server groups: %v", err)
	}
	if groupList == nil {
		return resources, nil
	}
	for _, group := range groupList.Groups {
		for _, version := range group.Versions {
			//get all resources for each group version
			resourceList, err := dc.ServerResourcesForGroupVersion(version.GroupVersion)
			if err != nil {
				logger.Error(err, "failed to get server resources")
				continue
			}
			if resourceList == nil || group.Name == "" {
				// don't want any resource with no apigroup
				continue
			}
			for _, resource := range resourceList.APIResources {

				resourceKind := strings.ToLower(resource.Kind)
				resourceName := resourceKind + "." + group.Name

				if !findValue(veleroBackup.Spec.ExcludedResources, resourceName) &&
					!findValue(veleroBackup.Spec.ExcludedResources, resourceKind) {
					resources = appendUnique(resources, resourceName)
				}
			}
		}
	}

	return resources, nil
}

// return hub uid, used to annotate backup schedules
// to know what hub is pushing the backups to the storage location
// info used when switching active - passive clusters
func getHubIdentification(
	ctx context.Context,
	dc discovery.DiscoveryInterface,
	dyn dynamic.Interface,
	mapper *restmapper.DeferredDiscoveryRESTMapper,
) (string, error) {

	uid := "unknown"
	logger := log.FromContext(ctx)
	groupKind := schema.GroupKind{
		Group: "config.openshift.io",
		Kind:  "ClusterVersion",
	}
	mapping, err := mapper.RESTMapping(groupKind, "")
	if err != nil {
		logger.Info(fmt.Sprintf("Failed to get dynamic mapper for group=%s, error : %s",
			groupKind, err.Error()))
		return uid, err
	}
	var dr = dyn.Resource(mapping.Resource)
	if dr == nil {
		return uid, nil
	}
	items, err := dr.List(ctx, v1.ListOptions{})
	if err != nil || items == nil || len(items.Items) == 0 ||
		items.Items[0].Object == nil {
		return uid, err
	}

	// get clusterID from the spec section
	specInfo := items.Items[0].Object["spec"]
	if specInfo != nil {
		iter := reflect.ValueOf(specInfo).MapRange()
		for iter.Next() {
			key := iter.Key().Interface()
			if key == "clusterID" {
				value := iter.Value().Interface()
				uid = value.(string)
			}

		}
	}
	return uid, nil
}
