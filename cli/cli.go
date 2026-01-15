package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/steebchen/prisma-client-go/binaries"
	"github.com/steebchen/prisma-client-go/binaries/platform"
	"github.com/steebchen/prisma-client-go/logger"
)

// Run the prisma CLI with given arguments
func Run(arguments []string, output bool) error {
	logger.Debug.Printf("running cli with args %+v", arguments)
	// TODO respect initial PRISMA_<name>_BINARY env
	// TODO optionally override CLI filepath using PRISMA_CLI_PATH

	dir := binaries.GlobalCacheDir()

	if err := binaries.FetchNative(dir); err != nil {
		return fmt.Errorf("could not fetch binaries: %w", err)
	}

	prisma := binaries.PrismaCLIName()

	// Handle shim for schema compatibility
	var cleanup func()
	var err error
	arguments, cleanup, err = shimSchemaCompatibility(arguments)
	if err != nil {
		return fmt.Errorf("failed to shim schema: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	logger.Debug.Printf("running %s %+v", path.Join(dir, prisma), arguments)

	cmd := exec.Command(path.Join(dir, prisma), arguments...) //nolint:gosec
	binaryName := platform.CheckForExtension(platform.Name(), platform.BinaryPlatformNameStatic())

	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "PRISMA_HIDE_UPDATE_MESSAGE=true")
	cmd.Env = append(cmd.Env, "PRISMA_CLI_QUERY_ENGINE_TYPE=binary")

	for _, engine := range binaries.Engines {
		var value string

		if env := os.Getenv(engine.Env); env != "" {
			logger.Debug.Printf("overriding %s to %s", engine.Name, env)
			value = env
		} else {
			value = path.Join(dir, binaries.EngineVersion, fmt.Sprintf("prisma-%s-%s", engine.Name, binaryName))
		}

		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", engine.Env, value))
	}

	cmd.Stdin = os.Stdin

	if output {
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not run %+v: %w", arguments, err)
	}

	return nil
}

// shimSchemaCompatibility checks for a schema missing the 'url' property in the datasource block
// and injects `url = env("DB_URL")` via a temporary file if needed.
func shimSchemaCompatibility(args []string) ([]string, func(), error) {
	schemaPath := findSchemaPath(args)
	if schemaPath == "" {
		// If no schema path is found, we can't do anything.
		// It might be using default locations which we could check,
		// but for now, let's rely on what we can find.
		// Actually, if it is using default locations, we should probably check them too
		// to be consistent.
		// However, finding the schema path from args is the most reliable way if provided.
		// If not provided, let's try to find it in default locations.
		defaultPaths := []string{"./schema.prisma", "./prisma/schema.prisma"}
		for _, p := range defaultPaths {
			if _, err := os.Stat(p); err == nil {
				schemaPath = p
				break
			}
		}
	}

	if schemaPath == "" {
		return args, nil, nil
	}

	content, err := os.ReadFile(schemaPath)
	if err != nil {
		// If we can't read the file, just proceed as is.
		return args, nil, nil
	}

	schemaStr := string(content)

	// Simple regex to find the datasource block
	// datasource db {
	//   provider = "..."
	// }
	datasourceRegex := regexp.MustCompile(`(?s)datasource\s+\w+\s+\{([^}]+)\}`)
	match := datasourceRegex.FindStringSubmatchIndex(schemaStr)

	if len(match) < 4 {
		return args, nil, nil
	}

	// match[2] and match[3] capture the content inside the brace
	blockStart, blockEnd := match[2], match[3]
	blockContent := schemaStr[blockStart:blockEnd]

	// Check if 'url' is present in the block
	// We look for 'url\s*='
	urlRegex := regexp.MustCompile(`\burl\s*=`)
	if urlRegex.MatchString(blockContent) {
		// url exists, no need to shim
		return args, nil, nil
	}

	// Inject url = env("DB_URL")
	logger.Info.Printf("Injected url = env(\"DB_URL\") into datasource block for compatibility.")

	// We insert it at the beginning of the block content
	newSchemaStr := schemaStr[:blockStart] + "\n  url = env(\"DB_URL\")" + schemaStr[blockStart:]

	// Create temp file
	tmpFile, err := os.CreateTemp("", "schema-*.prisma")
	if err != nil {
		return args, nil, fmt.Errorf("could not create temp schema file: %w", err)
	}

	if _, err := tmpFile.WriteString(newSchemaStr); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return args, nil, fmt.Errorf("could not write to temp schema file: %w", err)
	}
	tmpFile.Close()

	// Update args to point to the new schema
	newArgs := make([]string, len(args))
	copy(newArgs, args)

	found := false
	for i, arg := range newArgs {
		if arg == "--schema" && i+1 < len(newArgs) {
			newArgs[i+1] = tmpFile.Name()
			found = true
			break
		}
		if strings.HasPrefix(arg, "--schema=") {
			newArgs[i] = "--schema=" + tmpFile.Name()
			found = true
			break
		}
	}

	if !found {
		// If schema arg wasn't present, we need to append it.
		// But wait, the CLI commands usually take flags.
		// If we are injecting a schema file, we should make sure the command accepts it.
		// Most commands like validate, migrate, db push accept --schema.
		newArgs = append(newArgs, "--schema", tmpFile.Name())
	}

	cleanup := func() {
		os.Remove(tmpFile.Name())
	}

	return newArgs, cleanup, nil
}

func findSchemaPath(args []string) string {
	for i, arg := range args {
		if arg == "--schema" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--schema=") {
			return strings.TrimPrefix(arg, "--schema=")
		}
	}
	return ""
}
