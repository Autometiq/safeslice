# goreleaser supplies the prebuilt static binaries; nothing is compiled here.
#
# dockers_v2 stages the build context as <os>/<arch>/<binary>, one directory per
# target platform, so the COPY has to be platform-qualified. TARGETPLATFORM is
# set by buildx and resolves to the matching subdirectory (e.g. linux/arm64).
FROM gcr.io/distroless/static:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/safeslice /safeslice

# distroless/static rather than scratch: it carries CA certificates, which are
# required to reach a managed Postgres over sslmode=verify-full.
USER nonroot:nonroot
ENTRYPOINT ["/safeslice"]
