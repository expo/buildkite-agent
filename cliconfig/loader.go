package cliconfig

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/utils"
	"github.com/urfave/cli"
	"github.com/oleiade/reflections"
)

type Loader struct {
	// The context that is passed when using a codegangsta/cli action
	CLI *cli.Context

	// The struct that the config values will be loaded into
	Config interface{}

	// A slice of paths to files that should be used as config files
	DefaultConfigFilePaths []string

	// The file that was used when loading this configuration
	File *File
}

var argCliNameRegexp = regexp.MustCompile(`arg:(\d+)`)

// A shortcut for loading a config from the CLI
func Load(c *cli.Context, cfg interface{}) error {
	l := Loader{CLI: c, Config: cfg}

	return l.Load()
}

// Loads the config from the CLI and config files that are present.
func (l *Loader) Load() error {
	// Try and find a config file, either passed in the command line using
	// --config, or in one of the default configuration file paths.
	if l.CLI.String("config") != "" {
		file := File{Path: l.CLI.String("config")}

		// Because this file was passed in manually, we should throw an error
		// if it doesn't exist.
		if file.Exists() {
			l.File = &file
		} else {
			return fmt.Errorf("A configuration file could not be found at: %s", file.AbsolutePath())
		}
	} else if len(l.DefaultConfigFilePaths) > 0 {
		for _, path := range l.DefaultConfigFilePaths {
			file := File{Path: path}

			// If the config file exists, save it to the loader and
			// don't bother checking the others.
			if file.Exists() {
				l.File = &file
				break
			}
		}
	}

	// If a file was found, then we should load it
	if l.File != nil {
		// Attempt to load the config file we've found
		if err := l.File.Load(); err != nil {
			return err
		}
	}

	// Now it's onto actually setting the fields. We start by getting all
	// the fields from the configuration interface
	var fields []string
	fields, _ = reflections.Fields(l.Config)

	// Loop through each of the fields, and look for tags and handle them
	// appropriately
	for _, fieldName := range fields {
		// Start by loading the value from the CLI context if the tag
		// exists
		cliName, _ := reflections.GetFieldTag(l.Config, fieldName, "cli")
		if cliName != "" {
			// Load the value from the CLI Context
			err := l.setFieldValueFromCLI(fieldName, cliName)
			if err != nil {
				return err
			}
		}

		// Are there any normalizations we need to make?
		normalization, _ := reflections.GetFieldTag(l.Config, fieldName, "normalize")
		if normalization != "" {
			// Apply the normalization
			err := l.normalizeField(fieldName, normalization)
			if err != nil {
				return err
			}
		}

		// Check for field rename deprecations
		renamedToFieldName, _ := reflections.GetFieldTag(l.Config, fieldName, "deprecated-and-renamed-to")
		if renamedToFieldName != "" {
			// If the deprecated field's value isn't empty, then we
			// log a message, and set the proper config for them.
			if !l.fieldValueIsEmpty(fieldName) {
				renamedFieldCliName, _ := reflections.GetFieldTag(l.Config, renamedToFieldName, "cli")
				if renamedFieldCliName != "" {
					logger.Warn("The config option `%s` has been renamed to `%s`. Please update your configuration.", cliName, renamedFieldCliName)
				}

				// Fetch the value of the deprecated config, and set the renamed
				// config based on its value
				value, _ := reflections.GetField(l.Config, fieldName)
				if value != nil {
					err := reflections.SetField(l.Config, renamedToFieldName, value)
					if err != nil {
						return fmt.Errorf("Could not set value `%s` to field `%s` (%s)", value, renamedToFieldName, err)
					}
				}
			}
		}

		// Check for field deprecation
		deprecationError, _ := reflections.GetFieldTag(l.Config, fieldName, "deprecated")
		if deprecationError != "" {
			// If the deprecated field's value isn't empty, then we
			// return the deprecation error message.
			if !l.fieldValueIsEmpty(fieldName) {
				return fmt.Errorf(deprecationError)
			}
		}

		// Perform validations
		validationRules, _ := reflections.GetFieldTag(l.Config, fieldName, "validate")
		if validationRules != "" {
			// Determine the label for the field
			label, _ := reflections.GetFieldTag(l.Config, fieldName, "label")
			if label == "" {
				// Use the cli name if it exists, but if it
				// doesn't, just default to the structs field
				// name. Not great, but works!
				if cliName != "" {
					label = cliName
				} else {
					label = fieldName
				}
			}

			// Validate the fieid, and if it fails, return it's
			// error.
			err := l.validateField(fieldName, label, validationRules)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (l Loader) setFieldValueFromCLI(fieldName string, cliName string) error {
	// Get the kind of field we need to set
	fieldKind, err := reflections.GetFieldKind(l.Config, fieldName)
	if err != nil {
		return fmt.Errorf(`Failed to get the type of struct field %s`, fieldName)
	}

	var value interface{}

	// See the if the cli option is using the arg format i.e. (arg:1)
	argMatch := argCliNameRegexp.FindStringSubmatch(cliName)
	if len(argMatch) > 0 {
		argNum := argMatch[1]

		// Convert the arg position to an integer
		argIndex, err := strconv.Atoi(argNum)
		if err != nil {
			return fmt.Errorf("Failed to convert string to int: %s", err)
		}

		// Only set the value if the args are long enough for
		// the position to exist.
		if len(l.CLI.Args()) > argIndex {
			value = l.CLI.Args()[argIndex]
		}

		// Otherwise see if we can pull it from an environment variable
		// (and fail gracefuly if we can't)
		if value == nil {
			envName, err := reflections.GetFieldTag(l.Config, fieldName, "env")
			if err == nil {
				if envValue, envSet := os.LookupEnv(envName); envSet {
					value = envValue
				}
			}
		}
	} else {
		// If the cli name didn't have the special format, then we need to
		// either load from the context's flags, or from a config file.

		// We start by defaulting the value to what ever was provided
		// by the configuration file
		if l.File != nil {
			if configFileValue, ok := l.File.Config[cliName]; ok {
				// Convert the config file value to it's correct type
				if fieldKind == reflect.String {
					value = configFileValue
				} else if fieldKind == reflect.Slice {
					value = strings.Split(configFileValue, ",")
				} else if fieldKind == reflect.Bool {
					value, _ = strconv.ParseBool(configFileValue)
				} else if fieldKind == reflect.Int {
					value, _ = strconv.Atoi(configFileValue)
				} else {
					return fmt.Errorf("Unable to convert string to type %s", fieldKind)
				}
			}
		}

		// If a value hasn't been found in a config file, but there
		// _is_ one provided by the CLI context, then use that.
		if value == nil || l.cliValueIsSet(cliName) {
			if fieldKind == reflect.String {
				value = l.CLI.String(cliName)
			} else if fieldKind == reflect.Slice {
				value = l.CLI.StringSlice(cliName)
			} else if fieldKind == reflect.Bool {
				value = l.CLI.Bool(cliName)
			} else if fieldKind == reflect.Int {
				value = l.CLI.Int(cliName)
			} else {
				return fmt.Errorf("Unable to handle type: %s", fieldKind)
			}
		}
	}

	// Set the value to the cfg
	if value != nil {
		err = reflections.SetField(l.Config, fieldName, value)
		if err != nil {
			return fmt.Errorf("Could not set value `%s` to field `%s` (%s)", value, fieldName, err)
		}
	}

	return nil
}

func (l Loader) Errorf(format string, v ...interface{}) error {
	suffix := fmt.Sprintf(" See: `%s %s --help`", l.CLI.App.Name, l.CLI.Command.Name)

	return fmt.Errorf(format+suffix, v...)
}

func (l Loader) cliValueIsSet(cliName string) bool {
	if l.CLI.IsSet(cliName) {
		return true
	} else {
		// cli.Context#IsSet only checks to see if the command was set via the cli, not
		// via the environment. So here we do some hacks to find out the name of the
		// EnvVar, and return true if it was set.
		for _, flag := range l.CLI.Command.Flags {
			name, _ := reflections.GetField(flag, "Name")
			envVar, _ := reflections.GetField(flag, "EnvVar")
			if name == cliName && envVar != "" {
				// Make sure envVar is a string
				if envVarStr, ok := envVar.(string); ok {
					envVarStr = strings.TrimSpace(string(envVarStr))

					return os.Getenv(envVarStr) != ""
				}
			}
		}
	}

	return false
}

func (l Loader) fieldValueIsEmpty(fieldName string) bool {
	// We need to use the field kind to determine the type of empty test.
	value, _ := reflections.GetField(l.Config, fieldName)
	fieldKind, _ := reflections.GetFieldKind(l.Config, fieldName)

	if fieldKind == reflect.String {
		return value == ""
	} else if fieldKind == reflect.Slice {
		v := reflect.ValueOf(value)
		return v.Len() == 0
	} else if fieldKind == reflect.Bool {
		return value == false
	} else {
		panic(fmt.Sprintf("Can't determine empty-ness for field type %s", fieldKind))
	}

	return false
}

func (l Loader) validateField(fieldName string, label string, validationRules string) error {
	// Split up the validation rules
	rules := strings.Split(validationRules, ",")

	// Loop through each rule, and perform it
	for _, rule := range rules {
		if rule == "required" {
			if l.fieldValueIsEmpty(fieldName) {
				return l.Errorf("Missing %s.", label)
			}
		} else if rule == "file-exists" {
			value, _ := reflections.GetField(l.Config, fieldName)

			// Make sure the value is converted to a string
			if valueAsString, ok := value.(string); ok {
				// Return an error if the path doesn't exist
				if _, err := os.Stat(valueAsString); err != nil {
					return fmt.Errorf("Could not find %s located at %s", label, value)
				}
			}
		} else {
			return fmt.Errorf("Unknown config validation rule `%s`", rule)
		}
	}

	return nil
}

func (l Loader) normalizeField(fieldName string, normalization string) error {
	if normalization == "filepath" {
		value, _ := reflections.GetField(l.Config, fieldName)
		fieldKind, _ := reflections.GetFieldKind(l.Config, fieldName)

		// Make sure we're normalizing a string filed
		if fieldKind != reflect.String {
			return fmt.Errorf("filepath normalization only works on string fields")
		}

		// Normalize the field to be a filepath
		if valueAsString, ok := value.(string); ok {
			normalizedPath := utils.NormalizeFilePath(valueAsString)
			if err := reflections.SetField(l.Config, fieldName, normalizedPath); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("Unknown normalization `%s`", normalization)
	}

	return nil
}
