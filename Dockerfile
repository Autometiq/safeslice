# goreleaser supplies the prebuilt static binary; nothing is compiled here.
FROM gcr.io/distroless/static:nonroot
COPY safeslice /safeslice
USER nonroot:nonroot
ENTRYPOINT ["/safeslice"]
