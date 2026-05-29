// Package repourl produces well-formed URLs for content hosted in a
// GitHub repository. It's pure string formatting with no dependency
// on the gh CLI — the thermo-nuclear sweep flagged its previous home
// in internal/github as a category mismatch (that package wraps the
// gh CLI; this is just URL templating).
package repourl

import "fmt"

// Raw builds a raw.githubusercontent.com URL for a file at a branch
// in the given owner/name repo. branch defaults to "main" when empty.
func Raw(repo, branch, path string) string {
	if branch == "" {
		branch = "main"
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, branch, path)
}
