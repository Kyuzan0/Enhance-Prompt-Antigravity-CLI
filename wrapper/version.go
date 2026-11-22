package main

// WrapperVersion is the SemVer release version synced from ../package.json
// (and overridable at build time via -ldflags "-X main.WrapperVersion=X.Y.Z").
var WrapperVersion = "2.1.0"

const WrapperAuthor = "Kyuzan0 (https://github.com/Kyuzan0)"
