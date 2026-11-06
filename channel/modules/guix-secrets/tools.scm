;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets tools)
  #:use-module (guix store)
  #:use-module (guix derivations)
  #:use-module (guix packages)
  #:use-module (guix build utils)
  #:use-module (ice-9 popen)
  #:use-module (ice-9 textual-ports)
  #:export (tool command-output checked-command spawn-logged))

(define (spawn-logged program log . args)
  "Start a foreground tool, capturing its diagnostics in a private log."
  (let ((pid (primitive-fork)))
    (when (zero? pid)
      (catch #t
        (lambda ()
          (let ((port (open-file log "w")))
            (chmod port #o600)
            (dup2 (fileno port) 1)
            (dup2 (fileno port) 2)
            (close-port port)
            (apply execl program program args)))
        (lambda _ (primitive-exit 127))))
    pid))

(define (tool package relative)
  "Find a tool on PATH, or realize the pinned Guix package containing it."
  (or (which (basename relative))
      (with-store store
        (let ((drv (package-derivation store package)))
          (build-derivations store (list drv))
          (string-append (derivation->output-path drv) "/" relative)))))

(define (checked-command program . args)
  (unless (zero? (apply system* program args))
    (error "provisioning tool failed" (basename program))))

(define (command-output program . args)
  (let* ((pipe (apply open-pipe* OPEN_READ program args))
         (output (get-string-all pipe))
         (status (close-pipe pipe)))
    (unless (zero? status) (error "image inspection failed" (basename program)))
    output))
