package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	sourcePath := flag.String("source", "", "Path to the existing config.yaml")
	templatePath := flag.String("template", "", "Path to the new template config.yaml (upstream)")
	outputPath := flag.String("output", "", "Path to save the merged config (defaults to source)")
	flag.Parse()

	if *sourcePath == "" || *templatePath == "" {
		flag.Usage()
		os.Exit(1)
	}

	if *outputPath == "" {
		*outputPath = *sourcePath
	}

	// 1. Load source
	sourceData, err := ioutil.ReadFile(*sourcePath)
	if err != nil {
		log.Fatalf("Failed to read source config: %v", err)
	}

	var sourceNode yaml.Node
	if err := yaml.Unmarshal(sourceData, &sourceNode); err != nil {
		log.Fatalf("Failed to parse source config: %v", err)
	}

	// 2. Load template
	templateData, err := ioutil.ReadFile(*templatePath)
	if err != nil {
		log.Fatalf("Failed to read template config: %v", err)
	}

	var templateNode yaml.Node
	if err := yaml.Unmarshal(templateData, &templateNode); err != nil {
		log.Fatalf("Failed to parse template config: %v", err)
	}

	// 3. Merge recursively
	// We want to keep values from source, but add missing keys from template.
	if len(sourceNode.Content) > 0 && len(templateNode.Content) > 0 {
		mergeNodes(sourceNode.Content[0], templateNode.Content[0])
	}

	// 4. Save
	out, err := yaml.Marshal(&sourceNode)
	if err != nil {
		log.Fatalf("Failed to marshal merged config: %v", err)
	}

	if err := ioutil.WriteFile(*outputPath, out, 0644); err != nil {
		log.Fatalf("Failed to write merged config: %v", err)
	}

	fmt.Printf("Successfully merged configuration into %s\n", *outputPath)
}

func mergeNodes(source, template *yaml.Node) {
	if source.Kind != yaml.MappingNode || template.Kind != yaml.MappingNode {
		return
	}

	// Map of existing keys in source
	sourceKeys := make(map[string]*yaml.Node)
	for i := 0; i < len(source.Content); i += 2 {
		sourceKeys[source.Content[i].Value] = source.Content[i+1]
	}

	// Iterate through template keys
	for i := 0; i < len(template.Content); i += 2 {
		key := template.Content[i].Value
		templateVal := template.Content[i+1]

		if sourceVal, exists := sourceKeys[key]; exists {
			// Key exists, recurse if it's a mapping
			if sourceVal.Kind == yaml.MappingNode && templateVal.Kind == yaml.MappingNode {
				mergeNodes(sourceVal, templateVal)
			}
		} else {
			// Key missing in source, add it from template
			// We need to copy the key node and the value node
			keyNode := *template.Content[i]
			valNode := *template.Content[i+1]
			source.Content = append(source.Content, &keyNode, &valNode)
		}
	}
}
