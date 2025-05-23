// Copyright 2025 Redpanda Data, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"crypto/sha1"
	_ "embed"
	"encoding/json"
	"flag"
	"math/big"
	"strings"
	"text/template"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	generatedFilenameExtension = ".pb.mcp.go"
)

func main() {
	var flagSet flag.FlagSet

	protogen.Options{
		ParamFunc: flagSet.Set,
	}.Run(func(gen *protogen.Plugin) error {
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			newFileGenerator(f, gen).Generate()
		}
		return nil

	})
}

type fileGenerator struct {
	f   *protogen.File
	gen *protogen.Plugin

	allConsts map[string]struct{}
	gf        *protogen.GeneratedFile
}

func newFileGenerator(f *protogen.File, gen *protogen.Plugin) *fileGenerator {
	gen.SupportedFeatures |= uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

	return &fileGenerator{f: f, gen: gen}
}

//go:embed gen.tmpl
var fileTemplate string

type tplParams struct {
	PackageName string
	SourcePath  string
	GoPackage   string
	Tools       map[string]mcp.Tool
	Services    map[string]map[string]Tool
}

type Tool struct {
	RequestType  string
	ResponseType string
	MCPTool      mcp.Tool
}

func kindToType(kind protoreflect.Kind) string {
	switch kind {
	case protoreflect.BoolKind:
		return "boolean"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "integer"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "string" // safely encode as string
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "number"
	case protoreflect.BytesKind:
		return "string"
	case protoreflect.EnumKind:
		return "string" // optionally add enum values here
	default:
		return "string"
	}
}

func isFieldRequired(fd protoreflect.FieldDescriptor) bool {
	if proto.HasExtension(fd.Options(), annotations.E_FieldBehavior) {
		behaviors := proto.GetExtension(fd.Options(), annotations.E_FieldBehavior).([]annotations.FieldBehavior)
		for _, behavior := range behaviors {
			if behavior == annotations.FieldBehavior_REQUIRED {
				return true
			}
		}
	}
	return false
}

func messageSchema(md protoreflect.MessageDescriptor) map[string]any {
	required := []string{}
	// Fields that are not oneOf
	normalFields := map[string]any{}
	// One entry per oneOf block in the message.
	oneOf := map[string][]map[string]any{}

	// Process all fields in the message descriptor
	for i := 0; i < md.Fields().Len(); i++ {
		nestedFd := md.Fields().Get(i)
		name := string(nestedFd.Name())

		// Check if the field is part of a oneof group
		if oneof := nestedFd.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() {
			if _, ok := oneOf[string(oneof.Name())]; !ok {
				oneOf[string(oneof.Name())] = []map[string]any{}
			}
			oneOf[string(oneof.Name())] = append(oneOf[string(oneof.Name())], map[string]any{
				"properties": map[string]any{
					name: getType(nestedFd),
				},
				"required": []string{name},
			})
		} else {
			// If not part of a oneof, handle as a normal field
			normalFields[name] = getType(nestedFd)
			if isFieldRequired(nestedFd) {
				required = append(required, name)
			}
		}
	}

	// OpenAPI works differently than protobuf, when it comes to oneOf.
	// In proto, not the oneOf name's field name is used, but the actual field name of the oneOf ENTRY.
	// Therefore, we use an anyOf, and add one oneOf entry per oneOf protobuf block.
	var anyOf []map[string]any
	for _, protoOneOf := range oneOf {
		anyOf = append(anyOf, map[string]any{
			"oneOf":    protoOneOf,
			"$comment": "In this schema, there is a oneOf group for evert protobuf oneOf block in the message.",
		})
	}

	// Final schema includes both properties and anyOf for flexibility
	result := map[string]any{
		"type":       "object",
		"properties": normalFields, // Regular properties defined
		"required":   required,
	}
	if anyOf != nil {
		result["anyOf"] = anyOf // Fields in properties are already allowed. anyOf is in addition - which covers all oneOf groups
	}
	return result
}

func getType(fd protoreflect.FieldDescriptor) map[string]any {
	var schema map[string]any
	if fd.IsMap() {
		// Add key constraints. Map keys in protobuf can have different primitive types, where JSON can use only string.
		keyType := fd.MapKey().Kind()
		keyConstraints := map[string]any{
			"type": "string",
		}
		switch keyType {
		case protoreflect.BoolKind:
			keyConstraints["enum"] = []string{"true", "false"}
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			keyConstraints["pattern"] = "^(0|[1-9]\\d*)$" // unsigned integers, no leading zeros
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
			keyConstraints["pattern"] = "^-?(0|[1-9]\\d*)$" // signed integers, no leading zeros
		default:
		}
		return map[string]any{
			"type":                 "object",
			"propertyNames":        keyConstraints,
			"additionalProperties": getType(fd.MapValue()),
		}
	} else if fd.Kind() == protoreflect.MessageKind {
		if fd.Kind() == protoreflect.MessageKind {
			fullName := string(fd.Message().FullName())

			switch fullName {
			case "google.protobuf.Timestamp":
				return map[string]any{
					"type":   "string",
					"format": "date-time",
				}
			case "google.protobuf.Duration":
				return map[string]any{
					"type":    "string",
					"pattern": `^-?[0-9]+(\.[0-9]+)?s$`,
				}
			case "google.protobuf.Struct":
				return map[string]any{
					"type":                 "object",
					"additionalProperties": true,
				}
			case "google.protobuf.Value":
				return map[string]any{}
			case "google.protobuf.ListValue":
				return map[string]any{
					"type":  "array",
					"items": map[string]any{},
				}
			case "google.protobuf.FieldMask":
				return map[string]any{
					"type": "string",
				}
			case "google.protobuf.Any":
				return map[string]any{
					"type": "object",
					"properties": map[string]any{
						"@type": map[string]any{
							"type": "string",
						},
						"value": map[string]any{},
					},
					"required": []string{"@type"},
				}
			case "google.protobuf.DoubleValue", "google.protobuf.FloatValue",
				"google.protobuf.Int32Value", "google.protobuf.UInt32Value":
				return map[string]any{
					"type":     "number",
					"nullable": true,
				}
			case "google.protobuf.Int64Value", "google.protobuf.UInt64Value":
				return map[string]any{
					"type":     "string",
					"nullable": true,
				}
			case "google.protobuf.StringValue":
				return map[string]any{
					"type":     "string",
					"nullable": true,
				}
			case "google.protobuf.BoolValue":
				return map[string]any{
					"type":     "boolean",
					"nullable": true,
				}
			case "google.protobuf.BytesValue":
				return map[string]any{
					"type":     "string",
					"format":   "byte",
					"nullable": true,
				}
			}
		}
		return messageSchema(fd.Message())
	} else if fd.Kind() == protoreflect.EnumKind {
		var values []string

		for i := 0; i < fd.Enum().Values().Len(); i++ {
			ev := fd.Enum().Values().Get(i)
			values = append(values, string(ev.Name()))
		}
		return map[string]any{
			"type": "string",
			"enum": values,
		}
	} else {
		schema = map[string]any{
			"type": kindToType(fd.Kind()),
		}
	}

	if fd.Kind() == protoreflect.BytesKind {
		schema["contentEncoding"] = "base64"
		schema["format"] = "byte"
	}

	// If array, wrap it with array type (and put actual schema into "items"
	if fd.IsList() {
		return map[string]any{
			"type":  "array",
			"items": schema,
		}
	}
	return schema
}

var strippedCommentPrefixes = []string{"buf:lint:", "@ignore-comment"}

func cleanComment(comment string) string {
	var cleanedLines []string
outer:
	for _, line := range strings.Split(comment, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, strip := range strippedCommentPrefixes {
			if strings.HasPrefix(trimmed, strip) {
				continue outer
			}
		}
		cleanedLines = append(cleanedLines, trimmed)
	}
	return strings.Join(cleanedLines, "\n")
}

func Base32String(b []byte) string {
	n := new(big.Int).SetBytes(b)
	return n.Text(36)
}

func MangleHeadIfTooLong(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}

	// Generate short hash of full name
	hash := sha1.Sum([]byte(name))
	hashPrefix := Base32String(hash[:])[:6] // e.g. "3fj92a"

	// Leave room for hash prefix + underscore
	available := maxLen - len(hashPrefix) - 1
	if available <= 0 {
		return hashPrefix
	}

	// Preserve the end of the name (most specific)
	tail := name[len(name)-available:]
	return hashPrefix + "_" + tail
}

func (g *fileGenerator) Generate() {
	file := g.f
	if len(g.f.Services) == 0 {
		return
	}
	goImportPath := file.GoImportPath

	g.gf = g.gen.NewGeneratedFile(
		file.GeneratedFilenamePrefix+generatedFilenameExtension,
		goImportPath,
	)
	fileTpl := fileTemplate
	tpl, err := template.New("gen").Parse(fileTpl)
	if err != nil {
		g.gen.Error(err)
		return
	}

	services := map[string]map[string]Tool{}
	tools := map[string]mcp.Tool{}

	for _, svc := range g.f.Services {
		s := map[string]Tool{}
		for _, meth := range svc.Methods {
			// Only unary supported at the moment
			if meth.Desc.IsStreamingClient() || meth.Desc.IsStreamingServer() {
				continue
			}
			methodName := string(meth.Desc.FullName())
			if nameSplit := strings.Split(string(meth.Desc.FullName()), "."); len(nameSplit) >= 2 {
				methodName = strings.Join(nameSplit[len(nameSplit)-2:], "_")
			}
			tool := mcp.Tool{
				Name:        MangleHeadIfTooLong(methodName, 64),
				Description: cleanComment(string(meth.Comments.Leading)),
			}

			m := messageSchema(meth.Input.Desc)
			marshaled, err := json.Marshal(m)
			if err != nil {
				panic(err)
			}
			tool.RawInputSchema = json.RawMessage(marshaled)

			s[meth.GoName] = Tool{
				RequestType:  g.gf.QualifiedGoIdent(meth.Input.GoIdent),
				ResponseType: g.gf.QualifiedGoIdent(meth.Output.GoIdent),
				MCPTool:      tool,
			}
			tools[svc.GoName+"_"+meth.GoName] = tool
		}
		services[string(svc.Desc.Name())] = s
	}

	params := tplParams{
		PackageName: string(g.f.Desc.Package()),
		SourcePath:  g.f.Desc.Path(),
		GoPackage:   string(g.f.GoPackageName),
		Services:    services,
		Tools:       tools,
	}
	err = tpl.Execute(g.gf, params)
	if err != nil {
		g.gen.Error(err)
	}
}
