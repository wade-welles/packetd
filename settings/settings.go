package settings

import (
	"encoding/json"
	"errors"
	"io/ioutil"
)

func GetSettings(segments []string) interface{} {
	var ok bool
	var err error
	var jsonObject map[string]interface{}

	jsonObject, err = readSettingsFile()
	if err != nil {
		return createErrorJsonError(err)
	}
	for i, value := range segments {
		if value != "" {
			j := jsonObject[value]
			if j == nil {
				return createErrorJsonString("Attribute " + value + " not found in JSON object")
			}
			// if final
			if i == (len(segments) - 1) {
				return j
			}
			jsonObject, ok = j.(map[string]interface{})
			if !ok {
				return createErrorJsonString("Cast error")
			}
		}
	}

	return jsonObject
}

func SetSettings(segments []string, jsonNewSettings interface{}) interface{} {
	var ok bool
	var err error
	var iterJsonObject map[string]interface{}
	var jsonExistingSettings map[string]interface{}

	jsonExistingSettings, err = readSettingsFile()
	if err != nil {
		return createErrorJsonError(err)
	}

	iterJsonObject = jsonExistingSettings

	if segments == nil {
		j, ok := jsonNewSettings.(map[string]interface{})
		if ok {
			jsonExistingSettings = j
		} else {
			return createErrorJsonString("Invalid global settings object")
		}
	} else {
		for i, value := range segments {
			//if this is the last value, set and break
			if i == len(segments)-1 {
				iterJsonObject[value] = jsonNewSettings
				break
			}

			// otherwise recurse down object
			// 3 cases:
			// if json[foo] does not exist, create a map
			// if json[foo] exists and is a map, recurse
			// if json[foo] exists and is not a map (its some value)
			//    in this case we overwrite with a map, and recurse

			if iterJsonObject[value] == nil {
				newMap := make(map[string]interface{})
				iterJsonObject[value] = newMap
				iterJsonObject = newMap
			} else {
				var j map[string]interface{}
				j, ok = iterJsonObject[value].(map[string]interface{})
				iterJsonObject[value] = make(map[string]interface{})
				if ok {
					iterJsonObject[value] = j
					iterJsonObject = j // for next iteration
				} else {
					newMap := make(map[string]interface{})
					iterJsonObject[value] = newMap // create new map
					iterJsonObject = newMap        // for next iteration
				}
			}
		}
	}

	// Marshal it back to a string (with ident)
	var jsonString []byte
	jsonString, err = json.MarshalIndent(jsonExistingSettings, "", "  ")
	if err != nil {
		return createErrorJsonError(err)
	}

	err = ioutil.WriteFile("/etc/config/settings.json", jsonString, 0644)
	if err != nil {
		return createErrorJsonError(err)
	}
	return createErrorJsonObject("result", "OK")
}

func readSettingsFile() (map[string]interface{}, error) {
	raw, err := ioutil.ReadFile("/etc/config/settings.json")
	if err != nil {
		return nil, err
	}
	var jsonObject interface{}
	err = json.Unmarshal(raw, &jsonObject)
	if err != nil {
		return nil, err
	}
	j, ok := jsonObject.(map[string]interface{})
	if ok {
		return j, nil
	} else {
		return nil, errors.New("Invalid settings file format")
	}
}

func createErrorJsonObject(key string, value string) map[string]interface{} {
	return map[string]interface{}{key: value}
}

func createErrorJsonError(e error) map[string]interface{} {
	return createErrorJsonObject("error", e.Error())
}

func createErrorJsonString(str string) map[string]interface{} {
	return createErrorJsonObject("error", str)
}
