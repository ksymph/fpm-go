package main

import (
	"archive/zip"
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// --- Constants & Globals ---

const (
	defaultSource = "https://nexus-dev.unstable.life/repository/stable/components.xml"
	configFile    = "fpm.cfg"
)

var (
	basePath    string
	sourceURL   string
	components  []*Component
	compMap     map[string]*Component
	client      = &http.Client{Timeout: 10 * time.Second}
	helpText    = `NAME:
    fpm - Flashpoint Component Manager (Linux Port)

USAGE:
    fpm <command> [<arguments>...]

COMMANDS:
    list [available|downloaded|updates] [verbose]
    info <component>
    download <component...>
    remove <component...>
    update [component...]
    path [value]
    source [value]
`
)

// --- Structs ---

type Component struct {
	ID           string
	Title        string
	Description  string
	URL          string
	Directory    string
	LastUpdated  string
	DownloadSize int64
	InstallSize  int64
	Hash         string
	Depends      []string
	Downloaded   bool
	Outdated     bool
	OldSize      int64 // For calculating diff during updates
}

// XML Parsing Structures
type xmlNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xmlNode  `xml:",any"`
	Content string     `xml:",chardata"`
}

// --- Main Entry ---

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println(helpText)
		os.Exit(0)
	}

	cmd := args[0]

	// Initialize Config
	initConfig()

	// Handle config commands that don't require fetching components
	if cmd == "path" {
		handlePath(args)
		return
	}
	if cmd == "source" {
		handleSource(args)
		return
	}

	// Fetch components for all other commands
	if err := getComponents(); err != nil {
		fatal(fmt.Sprintf("Error fetching components: %v", err))
	}

	switch cmd {
	case "list":
		handleList(args)
	case "info":
		if len(args) < 2 {
			fatal("At least one argument is required")
		}
		handleInfo(args[1])
	case "download":
		handleDownload(args[1:])
	case "remove":
		if len(args) < 2 {
			fatal("At least one argument is required")
		}
		handleRemove(args[1:])
	case "update":
		handleUpdate(args[1:])
	default:
		fmt.Println(helpText)
	}
}

// --- Handlers ---

func handlePath(args []string) {
	if len(args) > 1 {
		absPath, err := filepath.Abs(args[1])
		if err != nil {
			fatal("Invalid path")
		}
		basePath = absPath
		writeConfig()
	} else {
		fmt.Println(basePath)
	}
}

func handleSource(args []string) {
	if len(args) > 1 {
		sourceURL = args[1]
		writeConfig()
	} else {
		fmt.Println(sourceURL)
	}
}

func handleList(args []string) {
	filter := ""
	verbose := false

	for _, arg := range args[1:] {
		if arg == "verbose" {
			verbose = true
		} else {
			filter = arg
		}
	}

	if len(components) == 0 {
		fmt.Println("No components found. Please check your source URL or internet connection.")
		return
	}

	for _, c := range components {
		if filter == "available" && c.Downloaded {
			continue
		}
		if filter == "downloaded" && !c.Downloaded {
			continue
		}
		if filter == "updates" && !c.Outdated {
			continue
		}

		prefix := " "
		if c.Downloaded {
			if c.Outdated {
				prefix = "!"
			} else {
				prefix = "*"
			}
		}

		output := fmt.Sprintf("%s %s", prefix, c.ID)
		if verbose {
			output += fmt.Sprintf(" (%s)", c.Title)
		}
		fmt.Println(output)
	}
}

func handleInfo(id string) {
	c, exists := compMap[id]
	if !exists {
		fatal("Specified component does not exist")
	}

	fmt.Printf("ID:             %s\n", c.ID)
	fmt.Printf("Title:          %s\n", c.Title)
	fmt.Printf("Description:    %s\n", c.Description)
	fmt.Printf("Download size:  %s\n", formatBytes(c.DownloadSize))
	fmt.Printf("Install size:   %s\n", formatBytes(c.InstallSize))
	fmt.Printf("Last updated:   %s\n", c.LastUpdated)
	fmt.Printf("CRC32:          %s\n\n", c.Hash)

	if len(c.Depends) > 0 {
		fmt.Printf("Dependencies: \n  %s\n\n", strings.Join(c.Depends, "\n  "))
	}

	req := "No"
	if strings.HasPrefix(c.ID, "core-") {
		req = "Yes"
	}
	fmt.Printf("Required?       %s\n", req)

	down := "No"
	if c.Downloaded {
		down = "Yes"
	}
	fmt.Printf("Downloaded?     %s\n", down)

	if c.Downloaded {
		upToDate := "Yes"
		if c.Outdated {
			upToDate = "No"
		}
		fmt.Printf("Up-to-date?     %s\n", upToDate)
	}
}

func handleDownload(args []string) {
	toDownload := resolveQueue(args, func(c *Component) bool {
		return !c.Downloaded
	})

	if len(toDownload) == 0 {
		fmt.Println("No components to download")
		return
	}

	var dlSize, instSize int64
	fmt.Println(len(toDownload), "component(s) will be downloaded:")
	for _, c := range toDownload {
		fmt.Printf("  %s\n", c.ID)
		dlSize += c.DownloadSize
		instSize += c.InstallSize
	}
	fmt.Println()
	fmt.Printf("Estimated download size: %s\n", formatBytes(dlSize))
	fmt.Printf("Estimated install size:  %s\n\n", formatBytes(instSize))

	if !confirm("Is this OK?") {
		return
	}

	for _, c := range toDownload {
		if err := downloadComponent(c); err != nil {
			fmt.Printf("Failed to download %s: %v\n", c.ID, err)
		}
	}
	fmt.Printf("\nSuccessfully downloaded %d components\n", len(toDownload))
}

func handleRemove(args []string) {
	// For remove, we only explicitly remove what was asked
	var cleanList []*Component
	var removeSize int64

	for _, arg := range args {
		matches := findComponents(arg)
		if len(matches) == 0 {
			fmt.Printf("Component or category %s does not exist and will be skipped\n", arg)
			continue
		}
		for _, c := range matches {
			if !c.Downloaded {
				fmt.Printf("Component %s is not downloaded and will be skipped\n", c.ID)
			} else {
				cleanList = append(cleanList, c)
				removeSize += c.InstallSize
			}
		}
	}
	cleanList = unique(cleanList)

	if len(cleanList) == 0 {
		fmt.Println("No components to remove")
		return
	}

	fmt.Println(len(cleanList), "component(s) will be removed:")
	for _, c := range cleanList {
		fmt.Printf("  %s\n", c.ID)
	}
	fmt.Println()
	fmt.Printf("Estimated freed size: %s\n\n", formatBytes(removeSize))

	if !confirm("Is this OK?") {
		return
	}

	for _, c := range cleanList {
		removeComponent(c)
	}
	fmt.Printf("\nSuccessfully removed %d components\n", len(cleanList))
}

func handleUpdate(args []string) {
	var toUpdate, toDownload []*Component

	if len(args) > 0 {
		visited := make(map[string]bool)
		var recurse func(string, bool)
		recurse = func(id string, isDepend bool) {
			matches := findComponents(id)
			if len(matches) == 0 {
				if !isDepend {
					fmt.Printf("Component or category %s does not exist\n", id)
				}
				return
			}

			for _, c := range matches {
				if visited[c.ID] {
					continue
				}
				visited[c.ID] = true

				if !c.Downloaded {
					if isDepend {
						toDownload = append(toDownload, c)
					} else {
						fmt.Printf("Component %s is not downloaded and will be skipped\n", c.ID)
					}
				} else if !c.Outdated {
					if !isDepend {
						fmt.Printf("Component %s is already up-to-date and will be skipped\n", c.ID)
					}
				} else {
					toUpdate = append(toUpdate, c)
					for _, dep := range c.Depends {
						recurse(dep, true)
					}
				}
			}
		}

		for _, arg := range args {
			recurse(arg, false)
		}

	} else {
		// Update all
		for _, c := range components {
			if c.Downloaded && c.Outdated {
				toUpdate = append(toUpdate, c)
			}
			if strings.HasPrefix(c.ID, "core-") && !c.Downloaded {
				toDownload = append(toDownload, c)
			}
		}
	}

	toUpdate = unique(toUpdate)
	toDownload = unique(toDownload)

	if len(toUpdate) == 0 && len(toDownload) == 0 {
		fmt.Println("No components to update")
		return
	}

	var dlSize, changeSize int64

	if len(toUpdate) > 0 {
		fmt.Println(len(toUpdate), "component(s) will be updated:")
		for _, c := range toUpdate {
			fmt.Printf("  %s\n", c.ID)
			dlSize += c.DownloadSize
			changeSize += (c.InstallSize - c.OldSize)
		}
		fmt.Println()
	}

	if len(toDownload) > 0 {
		fmt.Println(len(toDownload), "component(s) will be downloaded:")
		for _, c := range toDownload {
			fmt.Printf("  %s\n", c.ID)
			dlSize += c.DownloadSize
			changeSize += c.InstallSize
		}
		fmt.Println()
	}

	fmt.Printf("Estimated download size: %s\n", formatBytes(dlSize))
	fmt.Printf("Estimated changed size:  %s\n\n", formatBytes(changeSize))

	if !confirm("Is this OK?") {
		return
	}

	for _, c := range toUpdate {
		removeComponent(c)
		if err := downloadComponent(c); err != nil {
			fmt.Printf("Failed to update %s: %v\n", c.ID, err)
		}
	}
	for _, c := range toDownload {
		if err := downloadComponent(c); err != nil {
			fmt.Printf("Failed to download %s: %v\n", c.ID, err)
		}
	}

	msg := fmt.Sprintf("\nSuccessfully updated %d components", len(toUpdate))
	if len(toDownload) > 0 {
		msg += fmt.Sprintf(" and downloaded %d components", len(toDownload))
	}
	fmt.Println(msg)
}

// --- Helpers ---

func initConfig() {
	// Set defaults
	ex, _ := os.Executable()
	basePath = filepath.Clean(filepath.Join(filepath.Dir(ex), ".."))
	sourceURL = defaultSource

	data, err := ioutil.ReadFile(configFile)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
			basePath = strings.TrimSpace(lines[0])
		}
		if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
			sourceURL = strings.TrimSpace(lines[1])
		}
	} else {
		writeConfig()
	}
}

func writeConfig() {
	content := fmt.Sprintf("%s\n%s", basePath, sourceURL)
	if err := ioutil.WriteFile(configFile, []byte(content), 0644); err != nil {
		fmt.Println("Warning: Could not write to fpm.cfg")
	}
}

func getComponents() error {
	resp, err := client.Get(sourceURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return err
	}

	// Extract Repo URL from root list attribute
	repoURL := ""
	for _, attr := range root.Attrs {
		if attr.Name.Local == "url" {
			repoURL = attr.Value
			if !strings.HasSuffix(repoURL, "/") {
				repoURL += "/"
			}
			break
		}
	}

	components = []*Component{}
	compMap = make(map[string]*Component)
	parseNodes(root.Nodes, "", repoURL)
	return nil
}

// parseNodes now recursively handles 'category' tags to correctly build the ID path
func parseNodes(nodes []xmlNode, parentID string, repoURL string) {
	for _, node := range nodes {
		name := node.XMLName.Local
		
		// Ensure we process categories, components, and nested lists
		if name == "component" || name == "category" || name == "list" {
			id := getAttr(node, "id")
			fullID := id

			// Append parentID if exists (e.g. core-server-gamezip)
			if parentID != "" && id != "" {
				fullID = parentID + "-" + id
			} else if parentID != "" {
				fullID = parentID
			}

			if name == "component" {
				c := &Component{
					ID:           fullID,
					Title:        getAttr(node, "title"),
					Description:  getAttr(node, "description"),
					Directory:    getAttr(node, "path"),
					Hash:         getAttr(node, "hash"),
					URL:          repoURL + fullID + ".zip",
				}

				if val, err := strconv.ParseInt(getAttr(node, "date-modified"), 10, 64); err == nil {
					c.LastUpdated = time.Unix(val, 0).Format("2006-01-02 15:04:05")
				}
				if val, err := strconv.ParseInt(getAttr(node, "download-size"), 10, 64); err == nil {
					c.DownloadSize = val
				}
				if val, err := strconv.ParseInt(getAttr(node, "install-size"), 10, 64); err == nil {
					c.InstallSize = val
				}
				depStr := getAttr(node, "depends")
				if depStr != "" {
					c.Depends = strings.Split(depStr, " ")
				}

				// Check local state
				infoPath := filepath.Join(basePath, "Components", c.ID)
				if _, err := os.Stat(infoPath); err == nil {
					c.Downloaded = true

					// Read header
					f, err := os.Open(infoPath)
					if err == nil {
						scanner := bufio.NewScanner(f)
						if scanner.Scan() {
							headerParts := strings.Split(scanner.Text(), " ")
							if len(headerParts) >= 2 {
								if headerParts[0] != c.Hash {
									c.Outdated = true
									c.OldSize, _ = strconv.ParseInt(headerParts[1], 10, 64)
								}
							}
						}
						f.Close()
					}
				}

				components = append(components, c)
				compMap[c.ID] = c
			}

			// Recurse for nested lists or categories
			parseNodes(node.Nodes, fullID, repoURL)
		}
	}
}

func getAttr(node xmlNode, name string) string {
	for _, attr := range node.Attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func resolveQueue(args []string, criteria func(*Component) bool) []*Component {
	var queue []*Component
	visited := make(map[string]bool)

	var add func(string)
	add = func(id string) {
		matches := findComponents(id)
		if len(matches) == 0 {
			fmt.Printf("Component or category %s does not exist\n", id)
			return
		}

		for _, c := range matches {
			if visited[c.ID] {
				continue
			}
			visited[c.ID] = true

			if criteria(c) {
				queue = append(queue, c)
				for _, dep := range c.Depends {
					add(dep)
				}
			}
		}
	}

	if len(args) == 0 {
		// All components
		for _, c := range components {
			add(c.ID)
		}
	} else {
		for _, arg := range args {
			add(arg)
		}
	}

	return unique(queue)
}

func findComponents(id string) []*Component {
	var matches []*Component
	for _, c := range components {
		if c.ID == id || strings.HasPrefix(c.ID, id+"-") {
			matches = append(matches, c)
		}
	}
	return matches
}

func unique(slice []*Component) []*Component {
	keys := make(map[string]bool)
	list := []*Component{}
	for _, entry := range slice {
		if _, value := keys[entry.ID]; !value {
			keys[entry.ID] = true
			list = append(list, entry)
		}
	}
	return list
}

func downloadComponent(c *Component) error {
	if c.InstallSize == 0 {
		return nil
	}

	fmt.Printf("Downloading %s... ", c.ID)

	resp, err := client.Get(c.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}

	// Create temp file for zip
	tmpFile, err := ioutil.TempFile("", "fpm-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return err
	}
	tmpFile.Close()

	fmt.Print("Extracting... ")

	// Extract
	r, err := zip.OpenReader(tmpFile.Name())
	if err != nil {
		return err
	}
	defer r.Close()

	installedFiles := []string{}
	// Header: HASH SIZE DEP1 DEP2...
	header := fmt.Sprintf("%s %d %s", c.Hash, c.InstallSize, strings.Join(c.Depends, " "))
	installedFiles = append(installedFiles, header)

	destDir := filepath.Join(basePath, filepath.FromSlash(c.Directory))
	os.MkdirAll(destDir, 0755)

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}

		fpath := filepath.Join(destDir, filepath.FromSlash(f.Name))

		// Zip Slip check
		if !strings.HasPrefix(fpath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", fpath)
		}

		os.MkdirAll(filepath.Dir(fpath), 0755)

		rc, err := f.Open()
		if err != nil {
			return err
		}

		outFile, err := os.Create(fpath)
		if err != nil {
			rc.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		// Record relative path for info file
		relPath := filepath.Join(filepath.FromSlash(c.Directory), filepath.FromSlash(f.Name))
		installedFiles = append(installedFiles, relPath)
	}

	// Write info file
	infoDir := filepath.Join(basePath, "Components")
	os.MkdirAll(infoDir, 0755)

	err = ioutil.WriteFile(filepath.Join(infoDir, c.ID), []byte(strings.Join(installedFiles, "\n")), 0644)
	if err != nil {
		fmt.Println("Warning: Could not write component info file")
	}

	fmt.Println("done!")
	return nil
}

func removeComponent(c *Component) {
	fmt.Printf("   Removing %s... ", c.ID)

	infoPath := filepath.Join(basePath, "Components", c.ID)
	data, err := ioutil.ReadFile(infoPath)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		// Skip header (index 0)
		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			fullPath := filepath.Join(basePath, line)
			fullDelete(fullPath)
		}
	}

	fullDelete(infoPath)
	fmt.Println("done!")
}

func fullDelete(path string) {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		// If it's a directory (from C# logic generic handler), try removing
		// but specifically C# logic does explicit file delete then folder cleanup
		return
	}

	// Clean up empty directories upwards
	dir := filepath.Dir(path)
	absBase, _ := filepath.Abs(basePath)
	absDir, _ := filepath.Abs(dir)

	for absDir != absBase && strings.HasPrefix(absDir, absBase) {
		f, err := os.Open(absDir)
		if err != nil {
			break
		}
		_, err = f.Readdirnames(1) // check if empty
		f.Close()

		if err == io.EOF { // Empty
			os.Remove(absDir)
			absDir = filepath.Dir(absDir)
		} else {
			break
		}
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func confirm(msg string) bool {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("%s [y/n]: ", msg)
		response, _ := reader.ReadString('\n')
		response = strings.ToLower(strings.TrimSpace(response))
		if response == "y" {
			return true
		}
		if response == "n" {
			return false
		}
	}
}

func fatal(msg string) {
	fmt.Printf("Error: %s\n", msg)
	os.Exit(1)
}
