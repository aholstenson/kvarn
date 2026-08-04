package local

// SweepStaleVMFiles is exported for unit tests. It removes VM temp files left
// in a directory by a previous orchestrator.
var SweepStaleVMFiles = sweepStaleVMFiles
