;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix extensions secrets)
  #:use-module (guix ui)
  #:use-module (guix scripts)
  #:use-module (guix utils)
  #:use-module (guix secrets)
  #:use-module (gnu machine)
  #:use-module (gnu image)
  #:use-module (gnu system)
  #:use-module (gnu system image)
  #:use-module (guix scripts system)
  #:autoload (guix-secrets image) (provision-image)
  #:autoload (guix-secrets deploy) (deploy-files)
  #:use-module (ice-9 getopt-long)
  #:use-module (ice-9 match)
  #:use-module (srfi srfi-1)
  #:export (guix-secrets resolve-image load-machines))

(define (show-help)
  (display "Usage: guix secrets image --secrets=FILE --artifact=IMAGE --output=IMAGE
                          [--image-type=TYPE] OS-OR-IMAGE.scm
       guix secrets deploy --secrets=FILE MACHINE.scm...

Provision files only. No system builds, reconfiguration or service restarts.
Secrets files evaluate to a (guix secrets) secrets-configuration record.
Image root and mount layout come from the declaration. Supports ext2/3/4 and
Btrfs in raw MBR/GPT artifacts. FUSE is preferred for ext filesystems;
kernel mounts use Polkit when the caller is unprivileged.
Deploy uses managed SSH machine configurations and an already privileged user.
  -L, --load-path=DIR   add a Guile module search path (repeatable)
      --system=SYSTEM  evaluate declarations for SYSTEM
      --target=TARGET  evaluate image declarations for cross TARGET
      --secrets-directory=DIR  base directory for generated secret declarations
  -h, --help            show this help
"))

(define (required opts name)
  (or (option-ref opts name #f) (error "missing required provisioning option" name)))

(define (resolve-image file type)
  (let ((spec (read-operating-system file)))
    (cond ((image? spec) spec)
          ((operating-system? spec)
           ;; Guix system image defaults to this type. Use its pinned native
           ;; constructor, not a private CLI options binding (which may be
           ;; optimized out of compiled Guix modules).
           (os->image spec #:type (if type
                                     (lookup-image-type-by-name (string->symbol type))
                                     mbr-hybrid-raw-image-type)))
          (else (error "expected an operating-system or image record")))))

(define (load-machines file)
  (let ((value (load* file '((gnu machine) (gnu machine ssh)))))
    (cond ((machine? value) (list value))
          ((and (list? value) (every machine? value)) value)
          (else (error "expected a machine or list of machines")))))

(define-command (guix-secrets . args)
  (synopsis "provision private files in built images or running hosts")
  (sigaction SIGTERM (lambda _ (throw 'provisioning-cancelled)))
  (sigaction SIGINT (lambda _ (throw 'provisioning-cancelled)))
  (catch #t
    (lambda ()
      (let* ((opts (getopt-long (cons "secrets" args)
                               '((help (single-char #\h))
                                 (load-path (single-char #\L) (value #t))
                                 (secrets (value #t)) (secrets-directory (value #t)) (artifact (value #t))
                                 (output (value #t)) (image-type (value #t))
                                 (system (value #t)) (target (value #t)))))
             (operands (option-ref opts '() '())))
        (for-each (lambda (entry)
                    (when (eq? (car entry) 'load-path)
                      (set! %load-path (cons (cdr entry) %load-path))))
                  (reverse opts))
        (if (or (option-ref opts 'help #f) (null? args))
            (show-help)
            (let ((evaluate
                   (lambda (thunk)
                     (parameterize ((%current-system (option-ref opts 'system (%current-system)))
                                    (%current-target-system (option-ref opts 'target (%current-target-system))))
                       (thunk)))))
              (let ((files (evaluate
                            (lambda () (load-secrets (required opts 'secrets)
                                                    (option-ref opts 'secrets-directory #f))))))
                (match operands
                  (("image" spec)
                   ;; Realize tools and the privileged helper for the runner,
                   ;; not the target architecture used to evaluate declarations.
                   (provision-image (required opts 'artifact) (required opts 'output)
                                    files (evaluate (lambda () (resolve-image spec (option-ref opts 'image-type #f))))
                                    #:evaluate evaluate))
                  (("deploy" specs ...)
                   (when (null? specs) (error "missing machine declaration"))
                   (deploy-files files (evaluate (lambda () (append-map load-machines specs)))))
                  (_ (error "expected image or deploy action"))))))))
    (lambda (key . args)
      ;; Exceptions from evaluating private Scheme may contain secret values.
      (if (and (eq? key 'secrets-error) (pair? args) (string? (car args)))
          (format (current-error-port) "guix secrets: ~a~%" (car args))
          (format (current-error-port) "guix secrets: provisioning failed (~a)~%" key))
      (exit 1))))
