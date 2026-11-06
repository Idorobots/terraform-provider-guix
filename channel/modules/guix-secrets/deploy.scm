;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets deploy)
  #:use-module (guix ssh)
  #:use-module (guix-secrets files)
  #:use-module (ssh session)
  #:use-module (ssh popen)
  #:use-module (ssh channel)
  #:use-module (ice-9 textual-ports)
  #:export (deploy-files installer-expression))

(define (installer-expression)
  ;; Only this public installer code appears in argv. Payloads use stdin, and
  ;; errors are deliberately generic so a failed read cannot print the payload.
  (string-append
   "(begin\n"
   (call-with-input-file (search-path %load-path "guix-secrets/files.scm") get-string-all)
   "\n(catch #t (lambda () (set-port-encoding! (current-input-port) \"UTF-8\") (install-payload \"/\" (read)) (display \"installed\\n\"))"
   " (lambda _ (display \"secret installation failed\\n\" (current-error-port)) (exit 1))))"))

(define* (deploy-files files host user port identity host-key)
  (validate-payload files)
  (let* ((session (open-ssh-session host #:user user #:port port
                                    #:identity identity #:host-key host-key))
         (command (append (if (string=? user "root") '() '("sudo" "-n" "--"))
                          (list "/run/current-system/profile/bin/guile"
                                "--no-auto-compile" "-c" (installer-expression))))
         (pipe #f))
    (dynamic-wind
      (const #t)
      (lambda ()
        (set! pipe (apply open-remote-pipe* session OPEN_BOTH command))
        (set-port-encoding! pipe "UTF-8")
        (write files pipe)
        (newline pipe)
        (force-output pipe)
        (channel-send-eof pipe)
        (let ((result (get-string-all pipe)))
          (unless (and (string=? result "installed\n")
                       (zero? (channel-get-exit-status pipe)))
            (error "remote secret installation failed"))))
      (lambda ()
        (when pipe (close-port pipe))
        (disconnect! session)))))
