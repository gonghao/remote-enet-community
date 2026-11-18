package trust

import "os"

func openPublicSnapshot(root *os.Root) (*os.File, error) {
	return root.Open("trust.json")
}
