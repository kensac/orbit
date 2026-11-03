package store

// CodePieces is codePieces as strings, for TestCodePiecesMatchTheDatabase.
func CodePieces(w string) []string {
	var out []string
	for _, pc := range codePieces(w) {
		out = append(out, w[pc[0]:pc[1]])
	}
	return out
}
