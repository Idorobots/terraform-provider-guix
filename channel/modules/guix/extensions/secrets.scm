;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix extensions secrets)
  #:use-module (guix ui)
  #:use-module (guix scripts)
  #:use-module (guix-secrets manifest)
  #:autoload (guix-secrets image) (provision-image)
  #:autoload (guix-secrets deploy) (deploy-files)
  #:use-module (ice-9 getopt-long)
  #:use-module (ice-9 match)
  #:export (guix-secrets))

(define (show-help)
  (display "Usage: guix secrets image --manifest=FILE --output=IMAGE [--root-label=LABEL] BASE-IMAGE
       guix secrets deploy --manifest=FILE --host=HOST --host-key=KEY --identity=FILE
                           [--user=root] [--port=22]

Install secret files only. No system builds, reconfiguration, service restarts or boot.
Manifests are data-only JSON: version 1, files array of source/destination/mode.
Modes default to 0600; files and new private parent directories are root-owned.
Images use FUSE when available, otherwise request Polkit authentication.
  -L, --load-path=DIR   add a Guile module search path
  -h, --help            show this help
"))

(define (required opts name)
  (or (option-ref opts name #f) (error "missing required provisioning option" name)))

(define-command (guix-secrets . args)
  (synopsis "provision private files in built images or running hosts")
  (sigaction SIGTERM (lambda _ (throw 'provisioning-cancelled)))
  (sigaction SIGINT (lambda _ (throw 'provisioning-cancelled)))
  (catch #t
    (lambda ()
      (let* ((opts (getopt-long (cons "secrets" args)
                               '((help (single-char #\h))
                                 (load-path (single-char #\L) (value #t))
                                 (manifest (value #t)) (output (value #t))
                                 (root-label (value #t)) (host (value #t))
                                 (user (value #t)) (port (value #t))
                                 (identity (value #t)) (host-key (value #t)))))
             (operands (option-ref opts '() '())))
        (when (option-ref opts 'load-path #f)
          (set! %load-path (cons (option-ref opts 'load-path #f) %load-path)))
        (if (or (option-ref opts 'help #f) (null? args))
            (show-help)
            (let ((files (load-manifest (required opts 'manifest))))
              (match operands
                (("image" input)
                 (provision-image input (required opts 'output) files
                                  (option-ref opts 'root-label #f)))
                (("deploy")
                 (let ((port (string->number (option-ref opts 'port "22"))))
                   (unless (and (integer? port) (<= 1 port 65535))
                     (error "invalid SSH port"))
                   (deploy-files files (required opts 'host)
                                 (option-ref opts 'user "root") port
                                 (required opts 'identity) (required opts 'host-key))))
                (_ (error "expected image or deploy action")))))))
    (lambda (key . args)
      ;; No exception printer: reader/I/O exceptions can contain payload data.
      (if (and (eq? key 'secrets-error) (pair? args) (string? (car args)))
          (format (current-error-port) "guix secrets: ~a~%" (car args))
          (format (current-error-port) "guix secrets: provisioning failed (~a)~%" key))
      (exit 1))))
