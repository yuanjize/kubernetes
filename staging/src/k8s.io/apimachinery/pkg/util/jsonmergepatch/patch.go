/*
Copyright 2017 The Kubernetes Authors.

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

package jsonmergepatch

import (
	"fmt"
	"reflect"

	"github.com/evanphx/json-patch"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
)

// Create a 3-way merge patch based-on JSON merge patch.
// Calculate addition-and-change patch between current and modified.
// Calculate deletion patch between original and modified.

/*
 1.根据current和 modified 算出来一个addAndChange patch，然后删除该patch中的null值
 2.根据origin和 modified 算出来一个delette patch，然后删除该patch中的非null值只留下null值
 3.判断addAndChange patch和delette patch是否有冲突，如果没有冲突就合并这两个patch并通过fns过滤后返回
 */
func CreateThreeWayJSONMergePatch(original, modified, current []byte, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	if len(original) == 0 {
		original = []byte(`{}`)
	}
	if len(modified) == 0 {
		modified = []byte(`{}`)
	}
	if len(current) == 0 {
		current = []byte(`{}`)
	}

	addAndChangePatch, err := jsonpatch.CreateMergePatch(current, modified)
	if err != nil {
		return nil, err
	}
	// Only keep addition and changes
	addAndChangePatch, addAndChangePatchObj, err := keepOrDeleteNullInJsonPatch(addAndChangePatch, false)
	if err != nil {
		return nil, err
	}

	deletePatch, err := jsonpatch.CreateMergePatch(original, modified)
	if err != nil {
		return nil, err
	}
	// Only keep deletion
	deletePatch, deletePatchObj, err := keepOrDeleteNullInJsonPatch(deletePatch, true)
	if err != nil {
		return nil, err
	}
    // 对比两个jsonpatch是否冲突,如果l和r有相同的key，但是key对应的value的类型或者数值不一样，这个就算冲突
	hasConflicts, err := mergepatch.HasConflicts(addAndChangePatchObj, deletePatchObj)
	if err != nil {
		return nil, err
	}
	if hasConflicts {
		return nil, mergepatch.NewErrConflict(mergepatch.ToYAMLOrError(addAndChangePatchObj), mergepatch.ToYAMLOrError(deletePatchObj))
	}

	// patch合并
	patch, err := jsonpatch.MergePatch(deletePatch, addAndChangePatch)
	if err != nil {
		return nil, err
	}

	var patchMap map[string]interface{}
	err = json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, fmt.Errorf("Failed to unmarshal patch for precondition check: %s", patch)
	}
	meetPreconditions, err := meetPreconditions(patchMap, fns...)
	if err != nil {
		return nil, err
	}
	if !meetPreconditions {
		return nil, mergepatch.NewErrPreconditionFailed(patchMap)
	}

	return patch, nil
}
// 是keepOrDeleteNullInObj的包装函数
// keepOrDeleteNullInJsonPatch takes a json-encoded byte array and a boolean.
// It returns a filtered object and its corresponding json-encoded byte array.
// It is a wrapper of func keepOrDeleteNullInObj
func keepOrDeleteNullInJsonPatch(patch []byte, keepNull bool) ([]byte, map[string]interface{}, error) {
	var patchMap map[string]interface{}
	err := json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, nil, err
	}
	filteredMap, err := keepOrDeleteNullInObj(patchMap, keepNull)
	if err != nil {
		return nil, nil, err
	}
	o, err := json.Marshal(filteredMap)
	return o, filteredMap, err
}
// keepNull是true的时候只保留m中数值的null的key，否则只保留m中非空的key
// keepOrDeleteNullInObj will keep only the null value and delete all the others,
// if keepNull is true. Otherwise, it will delete all the null value and keep the others.
func keepOrDeleteNullInObj(m map[string]interface{}, keepNull bool) (map[string]interface{}, error) {
	filteredMap := make(map[string]interface{})
	var err error
	for key, val := range m {
		switch {
		case keepNull && val == nil:
			filteredMap[key] = nil
		case val != nil:
			switch typedVal := val.(type) {
			case map[string]interface{}:
				// Explicitly-set empty maps are treated as values instead of empty patches
				if len(typedVal) == 0 {
					if !keepNull {
						filteredMap[key] = typedVal
					}
					continue
				}

				var filteredSubMap map[string]interface{}
				filteredSubMap, err = keepOrDeleteNullInObj(typedVal, keepNull)
				if err != nil {
					return nil, err
				}

				// If the returned filtered submap was empty, this is an empty patch for the entire subdict, so the key
				// should not be set
				if len(filteredSubMap) != 0 {
					filteredMap[key] = filteredSubMap
				}

			case []interface{}, string, float64, bool, int64, nil:
				// Lists are always replaced in Json, no need to check each entry in the list.
				if !keepNull {
					filteredMap[key] = val
				}
			default:
				return nil, fmt.Errorf("unknown type: %v", reflect.TypeOf(typedVal))
			}
		}
	}
	return filteredMap, nil
}
// patch如果满足fns，返回false
func meetPreconditions(patchObj map[string]interface{}, fns ...mergepatch.PreconditionFunc) (bool, error) {
	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchObj) {
			return false, fmt.Errorf("precondition failed for: %v", patchObj)
		}
	}
	return true, nil
}
